/*
 *
 * Copyright 2022 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package transport implements the xDS transport protocol functionality
// required by the xdsclient.
package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/buffer"
	"google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/pretty"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/xds/internal/xdsclient/bootstrap"
	"google.golang.org/grpc/xds/internal/xdsclient/load"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource"
	"google.golang.org/protobuf/types/known/anypb"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3adsgrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	v3discoverypb "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
)

type adsStream = v3adsgrpc.AggregatedDiscoveryService_StreamAggregatedResourcesClient

// Transport provides a resource-type agnostic implementation of the xDS
// transport protocol. At this layer, resource contents are supposed to be
// opaque blobs which should be be meaningful only to the xDS data model layer
// which is implemented by the `xdsresource` package.
//
// Under the hood, it owns the gRPC connection to a single management server and
// manages the lifecycle of ADS/LRS streams. It uses the xDS v3 transport
// protocol version.
type Transport struct {
	// These fields are initialized at creation time and are read-only afterwards.
	cc                  *grpc.ClientConn        // ClientConn to the mangement server.
	serverURI           string                  // URI of the management server.
	updateHandler       UpdateHandlerFunc       // Resource update handler. xDS data model layer.
	adsStreamErrHandler func(error)             // To report underlying stream errors.
	lrsStore            *load.Store             // Store returned to user for pushing loads.
	backoff             func(int) time.Duration // Backoff after stream failures.
	nodeProto           *v3corepb.Node          // Identifies the gRPC application.
	logger              *grpclog.PrefixLogger   // Prefix logger for transport logs.
	adsRunnerCancel     context.CancelFunc      // CancelFunc for the ADS goroutine.
	adsRunnerDoneCh     chan struct{}           // To notify exit of ADS goroutine.
	lrsRunnerDoneCh     chan struct{}           // To notify exit of LRS goroutine.

	// These channels enable synchronization amongst the different goroutines
	// spawned by the transport, and between asynchorous events resulting from
	// receipt of responses from the management server.
	adsStreamCh  chan adsStream    // New ADS streams are pushed here.
	adsRequestCh *buffer.Unbounded // Resource and ack requests are pushed here.

	// mu guards the following runtime state maintained by the transport.
	mu sync.Mutex
	// resources is map from resource type URL to the set of resource names
	// being requested for that type. When the ADS stream is restarted, the
	// transport requests all these resources again from the management server.
	resources map[string]map[string]bool
	// versions is a map from resource type URL to the most recently ACKed
	// version for that resource. Resource versions are a property of the
	// resource type and not the stream, and will not be reset upon stream
	// restarts.
	versions map[string]string
	// nonces is a map from resource type URL to the most recently received
	// nonce for that resource type. Nonces are a property of the ADS stream and
	// will be reset upon stream restarts.
	nonces map[string]string

	lrsMu           sync.Mutex         // Protects all LRS state.
	lrsCancelStream context.CancelFunc // CancelFunc for the LRS stream.
	lrsRefCount     int                // Reference count on the load store.
}

// UpdateHandlerFunc is the implementation at the xDS data model layer, which
// determines if the configuration received from the management server can be
// applied locally or not.
//
// A nil error is returned from this function when the data model layer believes
// that the received configuration is good and can be applied locally. This will
// cause the transport layer to send an ACK to the management server. A non-nil
// error is returned from this function when the data model layer believes
// otherwise, and this will cause the transport layer to send a NACK.
type UpdateHandlerFunc func(update ResourceUpdate) error

// ResourceUpdate is a representation of the configuration update received from
// the management server. It only contains fields which are useful to the data
// model layer, and layers above it.
type ResourceUpdate struct {
	// Resources is the list of resources received from the management server.
	Resources []*anypb.Any
	// URL is the resource type URL for the above resources.
	URL string
	// Version is the resource version, for the above resources, as specified by
	// the management server.
	Version string
}

// Options specifies configuration knobs used when creating a new Transport.
type Options struct {
	// ServerCfg contains all the configuration required to connect to the xDS
	// management server.
	ServerCfg bootstrap.ServerConfig
	// UpdateHandler is the component which makes ACK/NACK decisions based on
	// the received resources.
	//
	// Invoked inline and implementations must not block.
	UpdateHandler UpdateHandlerFunc
	// StreamErrorHandler provides a way for the transport layer to report
	// underlying stream errors. These can be bubbled all the way up to the user
	// of the xdsClient.
	//
	// Invoked inline and implementations must not block.
	StreamErrorHandler func(error)
	// Backoff controls the amount of time to backoff before recreating failed
	// ADS streams. If unspecified, a default exponential backoff implementation
	// is used. For more details, see:
	// https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md.
	Backoff func(retries int) time.Duration
	// Logger does logging with a prefix.
	Logger *grpclog.PrefixLogger
	// NodeProto contains the Node proto to be used in xDS requests. This will be
	// of type *v3corepb.Node.
	NodeProto *v3corepb.Node
}

// For overriding in unit tests.
var grpcDial = grpc.Dial

// New creates a new Transport.
func New(opts Options) (*Transport, error) {
	switch {
	case opts.ServerCfg.ServerURI == "":
		return nil, errors.New("missing server URI when creating a new transport")
	case opts.ServerCfg.Creds == nil:
		return nil, errors.New("missing credentials when creating a new transport")
	case opts.UpdateHandler == nil:
		return nil, errors.New("missing update handler when creating a new transport")
	case opts.StreamErrorHandler == nil:
		return nil, errors.New("missing stream error handler when creating a new transport")
	}

	// Dial the xDS management with the passed in credentials.
	dopts := []grpc.DialOption{
		opts.ServerCfg.Creds,
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// We decided to use these sane defaults in all languages, and
			// kicked the can down the road as far making these configurable.
			Time:    5 * time.Minute,
			Timeout: 20 * time.Second,
		}),
	}
	cc, err := grpcDial(opts.ServerCfg.ServerURI, dopts...)
	if err != nil {
		// An error from a non-blocking dial indicates something serious.
		return nil, fmt.Errorf("failed to create a transport to the management server %q: %v", opts.ServerCfg.ServerURI, err)
	}

	boff := opts.Backoff
	if boff == nil {
		boff = backoff.DefaultExponential.Backoff
	}
	ret := &Transport{
		cc:                  cc,
		serverURI:           opts.ServerCfg.ServerURI,
		updateHandler:       opts.UpdateHandler,
		adsStreamErrHandler: opts.StreamErrorHandler,
		lrsStore:            load.NewStore(),
		backoff:             boff,
		nodeProto:           opts.NodeProto,
		logger:              opts.Logger,

		adsStreamCh:     make(chan adsStream, 1),
		adsRequestCh:    buffer.NewUnbounded(),
		resources:       make(map[string]map[string]bool),
		versions:        make(map[string]string),
		nonces:          make(map[string]string),
		adsRunnerDoneCh: make(chan struct{}),
	}

	// This context is used for sending and receiving RPC requests and
	// responses. It is also used by all the goroutines spawned by this
	// Transport. Therefore, cancelling this context when the transport is
	// closed will essentially cancel any pending RPCs, and cause the goroutines
	// to terminate.
	ctx, cancel := context.WithCancel(context.Background())
	ret.adsRunnerCancel = cancel
	go ret.adsRunner(ctx)

	ret.logger.Infof("Created transport to server %q", ret.serverURI)
	return ret, nil
}

// resourceRequest wraps the resource type url and the resource names requested
// by the user of this transport.
type resourceRequest struct {
	resources []string
	url       string
}

// SendRequest sends out an ADS request for the provided resources of the
// specified resource type.
//
// The request is sent out asynchronously. If no valid stream exists at the time
// of processing this request, it is queued and will be sent out once a valid
// stream exists.
//
// If a successful response is received, the update handler callback provided at
// creation time is invoked. If an error is encountered, the stream error
// handler callback provided at creation time is invoked.
func (t *Transport) SendRequest(url string, resources []string) {
	t.adsRequestCh.Put(&resourceRequest{
		url:       url,
		resources: resources,
	})
}

func (t *Transport) newAggregatedDiscoveryServiceStream(ctx context.Context, cc *grpc.ClientConn) (adsStream, error) {
	// The transport retries the stream with an exponential backoff whenever the
	// stream breaks. But if the channel is broken, we don't want the backoff
	// logic to continuously retry the stream. Setting WaitForReady() blocks the
	// stream creation until the channel is READY.
	//
	// TODO(easwars): Make changes required to comply with A57:
	// https://github.com/grpc/proposal/blob/master/A57-xds-client-failure-mode-behavior.md
	return v3adsgrpc.NewAggregatedDiscoveryServiceClient(cc).StreamAggregatedResources(ctx, grpc.WaitForReady(true))
}

func (t *Transport) sendAggregatedDiscoveryServiceRequest(stream adsStream, resourceNames []string, resourceURL, version, nonce string, nackErr error) error {
	req := &v3discoverypb.DiscoveryRequest{
		Node:          t.nodeProto,
		TypeUrl:       resourceURL,
		ResourceNames: resourceNames,
		VersionInfo:   version,
		ResponseNonce: nonce,
	}
	if nackErr != nil {
		req.ErrorDetail = &statuspb.Status{
			Code: int32(codes.InvalidArgument), Message: nackErr.Error(),
		}
	}
	if err := stream.Send(req); err != nil {
		return fmt.Errorf("sending ADS request %s failed: %v", pretty.ToJSON(req), err)
	}
	t.logger.Debugf("ADS request sent: %v", pretty.ToJSON(req))
	return nil
}

func (t *Transport) recvAggregatedDiscoveryServiceResponse(stream adsStream) (resources []*anypb.Any, resourceURL, version, nonce string, err error) {
	resp, err := stream.Recv()
	if err != nil {
		return nil, "", "", "", fmt.Errorf("failed to read ADS response: %v", err)
	}
	t.logger.Infof("ADS response received, type: %v", resp.GetTypeUrl())
	t.logger.Debugf("ADS response received: %v", pretty.ToJSON(resp))
	return resp.GetResources(), resp.GetTypeUrl(), resp.GetVersionInfo(), resp.GetNonce(), nil
}

// adsRunner starts an ADS stream (and backs off exponentially, if the previous
// stream failed without receiving a single reply) and runs the sender and
// receiver routines to send and receive data from the stream respectively.
func (t *Transport) adsRunner(ctx context.Context) {
	defer close(t.adsRunnerDoneCh)

	go t.send(ctx)

	// TODO: start a goroutine monitoring ClientConn's connectivity state, and
	// report error (and log) when stats is transient failure.

	backoffAttempt := 0
	backoffTimer := time.NewTimer(0)
	for ctx.Err() == nil {
		select {
		case <-backoffTimer.C:
		case <-ctx.Done():
			backoffTimer.Stop()
			return
		}

		// We reset backoff state when we successfully receive at least one
		// message from the server.
		resetBackoff := func() bool {
			stream, err := t.newAggregatedDiscoveryServiceStream(ctx, t.cc)
			if err != nil {
				t.adsStreamErrHandler(err)
				t.logger.Warningf("ADS stream creation failed: %v", err)
				return false
			}
			t.logger.Infof("ADS stream created")

			select {
			case <-t.adsStreamCh:
			default:
			}
			t.adsStreamCh <- stream
			return t.recv(stream)
		}()

		if resetBackoff {
			backoffTimer.Reset(0)
			backoffAttempt = 0
		} else {
			backoffTimer.Reset(t.backoff(backoffAttempt))
			backoffAttempt++
		}
	}
}

// send is a separate goroutine for sending resource requests on the ADS stream.
//
// For every new stream received on the stream channel, all existing resources
// are re-requested from the management server.
//
// For every new resource request received on the resources channel, the
// resources map is updated (this ensures that resend will pick them up when
// there are new streams) and the appropriate request is sent out.
func (t *Transport) send(ctx context.Context) {
	var stream adsStream
	for {
		select {
		case <-ctx.Done():
			return
		case stream = <-t.adsStreamCh:
			if !t.sendExisting(stream) {
				// Send failed, clear the current stream. Attempt to resend will
				// only be made after a new stream is created.
				stream = nil
			}
		case u := <-t.adsRequestCh.Get():
			t.adsRequestCh.Load()

			var (
				resources           []string
				url, version, nonce string
				send                bool
				nackErr             error
			)
			switch update := u.(type) {
			case *resourceRequest:
				resources, url, version, nonce = t.processResourceRequest(update)
			case *ackRequest:
				resources, url, version, nonce, send = t.processAckRequest(update, stream)
				if !send {
					continue
				}
				nackErr = update.nackErr
			}
			if stream == nil {
				// There's no stream yet. Skip the request. This request
				// will be resent to the new streams. If no stream is
				// created, the watcher will timeout (same as server not
				// sending response back).
				continue
			}
			if err := t.sendAggregatedDiscoveryServiceRequest(stream, resources, url, version, nonce, nackErr); err != nil {
				t.logger.Warningf("ADS request for {resources: %q, url: %v, version: %q, nonce: %q} failed: %v", resources, url, version, nonce, err)
				// Send failed, clear the current stream.
				stream = nil
			}
		}
	}
}

// sendExisting sends out xDS requests for existing resources when recovering
// from a broken stream.
//
// We call stream.Send() here with the lock being held. It should be OK to do
// that here because the stream has just started and Send() usually returns
// quickly (once it pushes the message onto the transport layer) and is only
// ever blocked if we don't have enough flow control quota.
func (t *Transport) sendExisting(stream adsStream) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reset only the nonces map when the stream restarts.
	//
	// xDS spec says the following. See section:
	// https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol#ack-nack-and-resource-type-instance-version
	//
	// Note that the version for a resource type is not a property of an
	// individual xDS stream but rather a property of the resources themselves. If
	// the stream becomes broken and the client creates a new stream, the client’s
	// initial request on the new stream should indicate the most recent version
	// seen by the client on the previous stream
	t.nonces = make(map[string]string)

	for url, resources := range t.resources {
		if err := t.sendAggregatedDiscoveryServiceRequest(stream, mapToSlice(resources), url, t.versions[url], "", nil); err != nil {
			t.logger.Warningf("ADS request failed: %v", err)
			return false
		}
	}

	return true
}

// recv receives xDS responses on the provided ADS stream and branches out to
// message specific handlers. Returns true if at least one message was
// successfully received.
func (t *Transport) recv(stream adsStream) bool {
	msgReceived := false
	for {
		resources, url, rVersion, nonce, err := t.recvAggregatedDiscoveryServiceResponse(stream)
		if err != nil {
			t.adsStreamErrHandler(err)
			t.logger.Warningf("ADS stream is closed with error: %v", err)
			return msgReceived
		}
		msgReceived = true

		err = t.updateHandler(ResourceUpdate{
			Resources: resources,
			URL:       url,
			Version:   rVersion,
		})
		if xdsresource.ErrType(err) == xdsresource.ErrorTypeResourceTypeUnsupported {
			t.logger.Warningf("%v", err)
			continue
		}
		// If the data model layer returned an error, we need to NACK the
		// response in which case we need to set the version to the most
		// recently accepted version of this resource type.
		if err != nil {
			t.mu.Lock()
			t.adsRequestCh.Put(&ackRequest{
				url:     url,
				nonce:   nonce,
				stream:  stream,
				version: t.versions[url],
				nackErr: err,
			})
			t.mu.Unlock()
			t.logger.Warningf("Sending NACK for resource type: %v, version: %v, nonce: %v, reason: %v", url, rVersion, nonce, err)
			continue
		}
		t.adsRequestCh.Put(&ackRequest{
			url:     url,
			nonce:   nonce,
			stream:  stream,
			version: rVersion,
		})
		t.logger.Infof("Sending ACK for resource type: %v, version: %v, nonce: %v", url, rVersion, nonce)
	}
}

func mapToSlice(m map[string]bool) []string {
	ret := make([]string, 0, len(m))
	for i := range m {
		ret = append(ret, i)
	}
	return ret
}

func sliceToMap(ss []string) map[string]bool {
	ret := make(map[string]bool, len(ss))
	for _, s := range ss {
		ret[s] = true
	}
	return ret
}

// processResourceRequest pulls the fields needed to send out an ADS request.
// The resource type and the list of resources to request are provided by the
// user, while the version and nonce are maintained internally.
//
// The resources map, which keeps track of the resources being requested, is
// updated here. Any subsequent stream failure will re-request resources stored
// in this map.
//
// Returns the list of resources, resource type url, version and nonce.
func (t *Transport) processResourceRequest(req *resourceRequest) ([]string, string, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	resources := sliceToMap(req.resources)
	t.resources[req.url] = resources
	return req.resources, req.url, t.versions[req.url], t.nonces[req.url]
}

type ackRequest struct {
	url     string // Resource type URL.
	version string // NACK if version is an empty string.
	nonce   string
	nackErr error // nil for ACK, non-nil for NACK.
	// ACK/NACK are tagged with the stream it's for. When the stream is down,
	// all the ACK/NACK for this stream will be dropped, and the version/nonce
	// won't be updated.
	stream grpc.ClientStream
}

// processAckRequest pulls the fields needed to send out an ADS ACK. The nonces
// and versions map is updated.
//
// Returns the list of resources, resource type url, version, nonce, and an
// indication of whether an ACK should be sent on the wire or not.
func (t *Transport) processAckRequest(ack *ackRequest, stream grpc.ClientStream) ([]string, string, string, string, bool) {
	if ack.stream != stream {
		// If ACK's stream isn't the current sending stream, this means the ACK
		// was pushed to queue before the old stream broke, and a new stream has
		// been started since. Return immediately here so we don't update the
		// nonce for the new stream.
		return nil, "", "", "", false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Update the nonce irrespective of whether we send the ACK request on wire.
	// An up-to-date nonce is required for the next request.
	nonce := ack.nonce
	t.nonces[ack.url] = nonce

	s, ok := t.resources[ack.url]
	if !ok || len(s) == 0 {
		// We don't send the ACK request if there are no resources of this type
		// in our resources map. This can be either when the server sends
		// responses before any request, or the resources are removed while the
		// ackRequest was in queue). If we send a request with an empty
		// resource name list, the server may treat it as a wild card and send
		// us everything.
		return nil, "", "", "", false
	}
	resources := mapToSlice(s)

	// Update the versions map only when we plan to send an ACK.
	if ack.nackErr == nil {
		t.versions[ack.url] = ack.version
	}

	return resources, ack.url, ack.version, nonce, true
}

// Close closes the Transport and frees any associated resources.
func (t *Transport) Close() {
	t.adsRunnerCancel()
	<-t.adsRunnerDoneCh
	t.cc.Close()
}

// ChannelConnectivityStateForTesting returns the connectivity state of the gRPC
// channel to the management server.
//
// Only for testing purposes.
func (t *Transport) ChannelConnectivityStateForTesting() connectivity.State {
	return t.cc.GetState()
}
