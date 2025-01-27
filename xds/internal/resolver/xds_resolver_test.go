/*
 *
 * Copyright 2019 gRPC authors.
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
 *
 */

package resolver

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	xxhash "github.com/cespare/xxhash/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	xdscreds "google.golang.org/grpc/credentials/xds"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/envconfig"
	"google.golang.org/grpc/internal/grpcrand"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/grpctest"
	iresolver "google.golang.org/grpc/internal/resolver"
	"google.golang.org/grpc/internal/testutils"
	xdsbootstrap "google.golang.org/grpc/internal/testutils/xds/bootstrap"
	"google.golang.org/grpc/internal/testutils/xds/e2e"
	"google.golang.org/grpc/internal/wrr"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/xds/internal/balancer/clustermanager"
	"google.golang.org/grpc/xds/internal/balancer/ringhash"
	"google.golang.org/grpc/xds/internal/httpfilter"
	"google.golang.org/grpc/xds/internal/httpfilter/router"
	"google.golang.org/grpc/xds/internal/testutils/fakeclient"
	"google.golang.org/grpc/xds/internal/xdsclient"
	"google.golang.org/grpc/xds/internal/xdsclient/bootstrap"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource/version"
	"google.golang.org/protobuf/types/known/wrapperspb"

	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	v3routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	v3discoverypb "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"

	_ "google.golang.org/grpc/xds/internal/balancer/cdsbalancer" // To parse LB config
)

const (
	targetStr               = "target"
	routeStr                = "route"
	cluster                 = "cluster"
	defaultTestTimeout      = 10 * time.Second
	defaultTestShortTimeout = 100 * time.Microsecond
)

var target = resolver.Target{Endpoint: targetStr, URL: url.URL{Scheme: "xds", Path: "/" + targetStr}}

var routerFilter = xdsresource.HTTPFilter{Name: "rtr", Filter: httpfilter.Get(router.TypeURL)}
var routerFilterList = []xdsresource.HTTPFilter{routerFilter}

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

func (s) TestRegister(t *testing.T) {
	if resolver.Get(xdsScheme) == nil {
		t.Errorf("scheme %v is not registered", xdsScheme)
	}
}

// testClientConn is a fake implemetation of resolver.ClientConn that pushes
// state updates and errors returned by the resolver on to channels for
// consumption by tests.
type testClientConn struct {
	resolver.ClientConn
	stateCh *testutils.Channel
	errorCh *testutils.Channel
}

func (t *testClientConn) UpdateState(s resolver.State) error {
	t.stateCh.Send(s)
	return nil
}

func (t *testClientConn) ReportError(err error) {
	t.errorCh.Send(err)
}

func (t *testClientConn) ParseServiceConfig(jsonSC string) *serviceconfig.ParseResult {
	return internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(jsonSC)
}

func newTestClientConn() *testClientConn {
	return &testClientConn{
		stateCh: testutils.NewChannel(),
		errorCh: testutils.NewChannel(),
	}
}

// TestResolverBuilder_ClientCreationFails tests the case where xDS client
// creation fails, and verifies that xDS resolver build fails as well.
func (s) TestResolverBuilder_ClientCreationFails(t *testing.T) {
	// Override xDS client creation function and return an error.
	origNewClient := newXDSClient
	newXDSClient = func() (xdsclient.XDSClient, func(), error) {
		return nil, nil, errors.New("failed to create xDS client")
	}
	defer func() {
		newXDSClient = origNewClient
	}()

	// Build an xDS resolver and expect it to fail.
	builder := resolver.Get(xdsScheme)
	if builder == nil {
		t.Fatalf("resolver.Get(%v) returned nil", xdsScheme)
	}
	if _, err := builder.Build(target, newTestClientConn(), resolver.BuildOptions{}); err == nil {
		t.Fatalf("builder.Build(%v) succeeded when expected to fail", target)
	}
}

// TestResolverBuilder_DifferentBootstrapConfigs tests the resolver builder's
// Build() method with different xDS bootstrap configurations.
func (s) TestResolverBuilder_DifferentBootstrapConfigs(t *testing.T) {
	tests := []struct {
		name         string
		bootstrapCfg *bootstrap.Config // Empty top-level xDS server config, will be set by test logic.
		target       resolver.Target
		buildOpts    resolver.BuildOptions
		wantErr      string
	}{
		{
			name:         "good",
			bootstrapCfg: &bootstrap.Config{},
			target:       target,
		},
		{
			name: "authority not defined in bootstrap",
			bootstrapCfg: &bootstrap.Config{
				ClientDefaultListenerResourceNameTemplate: "%s",
				Authorities: map[string]*bootstrap.Authority{
					"test-authority": {
						ClientListenerResourceNameTemplate: "xdstp://test-authority/%s",
					},
				},
			},
			target: resolver.Target{
				URL: url.URL{
					Host: "non-existing-authority",
					Path: "/" + targetStr,
				},
			},
			wantErr: `authority "non-existing-authority" is not found in the bootstrap file`,
		},
		{
			name:         "xDS creds specified without certificate providers in bootstrap",
			bootstrapCfg: &bootstrap.Config{},
			target:       target,
			buildOpts: resolver.BuildOptions{
				DialCreds: func() credentials.TransportCredentials {
					creds, err := xdscreds.NewClientCredentials(xdscreds.ClientOptions{FallbackCreds: insecure.NewCredentials()})
					if err != nil {
						t.Fatalf("xds.NewClientCredentials() failed: %v", err)
					}
					return creds
				}(),
			},
			wantErr: `xdsCreds specified but certificate_providers config missing in bootstrap file`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mgmtServer, err := e2e.StartManagementServer(e2e.ManagementServerOptions{})
			if err != nil {
				t.Fatalf("Starting xDS management server: %v", err)
			}
			defer mgmtServer.Stop()

			// Add top-level xDS server config corresponding to the above
			// management server.
			test.bootstrapCfg.XDSServer = &bootstrap.ServerConfig{
				ServerURI:    mgmtServer.Address,
				Creds:        grpc.WithTransportCredentials(insecure.NewCredentials()),
				TransportAPI: version.TransportV3,
			}

			// Override xDS client creation to use bootstrap configuration
			// specified by the test.
			origNewClient := newXDSClient
			newXDSClient = func() (xdsclient.XDSClient, func(), error) {
				// The watch timeout and idle authority timeout values passed to
				// NewWithConfigForTesing() are immaterial for this test, as we
				// are only testing the resolver build functionality.
				return xdsclient.NewWithConfigForTesting(test.bootstrapCfg, defaultTestTimeout, defaultTestTimeout)
			}
			defer func() {
				newXDSClient = origNewClient
			}()

			builder := resolver.Get(xdsScheme)
			if builder == nil {
				t.Fatalf("resolver.Get(%v) returned nil", xdsScheme)
			}

			r, err := builder.Build(test.target, newTestClientConn(), test.buildOpts)
			if gotErr, wantErr := err != nil, test.wantErr != ""; gotErr != wantErr {
				t.Fatalf("builder.Build(%v) returned err: %v, wantErr: %v", target, err, test.wantErr)
			}
			if test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("builder.Build(%v) returned err: %v, wantErr: %v", target, err, test.wantErr)
			}
			if err != nil {
				// This is the case where we expect an error and got it.
				return
			}
			r.Close()
		})
	}
}

type setupOpts struct {
	bootstrapC *bootstrap.Config
	target     resolver.Target
}

func testSetup(t *testing.T, opts setupOpts) (*xdsResolver, *fakeclient.Client, *testClientConn, func()) {
	t.Helper()

	fc := fakeclient.NewClient()
	if opts.bootstrapC != nil {
		fc.SetBootstrapConfig(opts.bootstrapC)
	}
	oldClientMaker := newXDSClient
	closeCh := make(chan struct{})
	newXDSClient = func() (xdsclient.XDSClient, func(), error) {
		return fc, grpcsync.OnceFunc(func() { close(closeCh) }), nil
	}
	cancel := func() {
		// Make sure the xDS client is closed, in all (successful or failed)
		// cases.
		select {
		case <-time.After(defaultTestTimeout):
			t.Fatalf("timeout waiting for close")
		case <-closeCh:
		}
		newXDSClient = oldClientMaker
	}
	builder := resolver.Get(xdsScheme)
	if builder == nil {
		t.Fatalf("resolver.Get(%v) returned nil", xdsScheme)
	}

	tcc := newTestClientConn()
	r, err := builder.Build(opts.target, tcc, resolver.BuildOptions{})
	if err != nil {
		t.Fatalf("builder.Build(%v) returned err: %v", target, err)
	}
	return r.(*xdsResolver), fc, tcc, func() {
		r.Close()
		cancel()
	}
}

// waitForWatchListener waits for the WatchListener method to be called on the
// xdsClient within a reasonable amount of time, and also verifies that the
// watch is called with the expected target.
func waitForWatchListener(ctx context.Context, t *testing.T, xdsC *fakeclient.Client, wantTarget string) {
	t.Helper()

	gotTarget, err := xdsC.WaitForWatchListener(ctx)
	if err != nil {
		t.Fatalf("xdsClient.WatchService failed with error: %v", err)
	}
	if gotTarget != wantTarget {
		t.Fatalf("xdsClient.WatchService() called with target: %v, want %v", gotTarget, wantTarget)
	}
}

// waitForWatchRouteConfig waits for the WatchRoute method to be called on the
// xdsClient within a reasonable amount of time, and also verifies that the
// watch is called with the expected target.
func waitForWatchRouteConfig(ctx context.Context, t *testing.T, xdsC *fakeclient.Client, wantTarget string) {
	t.Helper()

	gotTarget, err := xdsC.WaitForWatchRouteConfig(ctx)
	if err != nil {
		t.Fatalf("xdsClient.WatchService failed with error: %v", err)
	}
	if gotTarget != wantTarget {
		t.Fatalf("xdsClient.WatchService() called with target: %v, want %v", gotTarget, wantTarget)
	}
}

// buildResolverForTarget builds an xDS resolver for the given target. It
// returns a testClientConn which allows inspection of resolver updates, and a
// function to close the resolver once the test is complete.
func buildResolverForTarget(t *testing.T, target resolver.Target) (*testClientConn, func()) {
	builder := resolver.Get(xdsScheme)
	if builder == nil {
		t.Fatalf("resolver.Get(%v) returned nil", xdsScheme)
	}

	tcc := newTestClientConn()
	r, err := builder.Build(target, tcc, resolver.BuildOptions{})
	if err != nil {
		t.Fatalf("builder.Build(%v) returned err: %v", target, err)
	}
	return tcc, r.Close
}

// TestResolverResourceName builds an xDS resolver and verifies that the
// resource name specified in the discovery request matches expectations.
func (s) TestResolverResourceName(t *testing.T) {
	// Federation support is required when new style names are used.
	oldXDSFederation := envconfig.XDSFederation
	envconfig.XDSFederation = true
	defer func() { envconfig.XDSFederation = oldXDSFederation }()

	tests := []struct {
		name                         string
		listenerResourceNameTemplate string
		extraAuthority               string
		dialTarget                   string
		wantResourceName             string
	}{
		{
			name:                         "default %s old style",
			listenerResourceNameTemplate: "%s",
			dialTarget:                   "xds:///target",
			wantResourceName:             "target",
		},
		{
			name:                         "old style no percent encoding",
			listenerResourceNameTemplate: "/path/to/%s",
			dialTarget:                   "xds:///target",
			wantResourceName:             "/path/to/target",
		},
		{
			name:                         "new style with %s",
			listenerResourceNameTemplate: "xdstp://authority.com/%s",
			dialTarget:                   "xds:///0.0.0.0:8080",
			wantResourceName:             "xdstp://authority.com/0.0.0.0:8080",
		},
		{
			name:                         "new style percent encoding",
			listenerResourceNameTemplate: "xdstp://authority.com/%s",
			dialTarget:                   "xds:///[::1]:8080",
			wantResourceName:             "xdstp://authority.com/%5B::1%5D:8080",
		},
		{
			name:                         "new style different authority",
			listenerResourceNameTemplate: "xdstp://authority.com/%s",
			extraAuthority:               "test-authority",
			dialTarget:                   "xds://test-authority/target",
			wantResourceName:             "xdstp://test-authority/envoy.config.listener.v3.Listener/target",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup the management server to push the requested resource name
			// on to a channel. No resources are configured on the management
			// server as part of this test, as we are only interested in the
			// resource name being requested.
			resourceNameCh := make(chan string, 1)
			mgmtServer, err := e2e.StartManagementServer(e2e.ManagementServerOptions{
				OnStreamRequest: func(_ int64, req *v3discoverypb.DiscoveryRequest) error {
					// When the resolver is being closed, the watch associated
					// with the listener resource will be cancelled, and it
					// might result in a discovery request with no resource
					// names. Hence, we only consider requests which contain a
					// resource name.
					var name string
					if len(req.GetResourceNames()) == 1 {
						name = req.GetResourceNames()[0]
					}
					select {
					case resourceNameCh <- name:
					default:
					}
					return nil
				},
			})
			if err != nil {
				t.Fatalf("Failed to start xDS management server: %v", err)
			}
			defer mgmtServer.Stop()

			// Create a bootstrap configuration with test options.
			opts := xdsbootstrap.Options{
				ServerURI: mgmtServer.Address,
				Version:   xdsbootstrap.TransportV3,
				ClientDefaultListenerResourceNameTemplate: tt.listenerResourceNameTemplate,
			}
			if tt.extraAuthority != "" {
				// In this test, we really don't care about having multiple
				// management servers. All we need to verify is whether the
				// resource name matches expectation.
				opts.Authorities = map[string]string{
					tt.extraAuthority: mgmtServer.Address,
				}
			}
			cleanup, err := xdsbootstrap.CreateFile(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			_, rClose := buildResolverForTarget(t, resolver.Target{URL: *testutils.MustParseURL(tt.dialTarget)})
			defer rClose()

			// Verify the resource name in the discovery request being sent out.
			select {
			case gotResourceName := <-resourceNameCh:
				if gotResourceName != tt.wantResourceName {
					t.Fatalf("Received discovery request with resource name: %v, want %v", gotResourceName, tt.wantResourceName)
				}
			case <-time.After(defaultTestTimeout):
				t.Fatalf("Timeout when waiting for discovery request")
			}
		})
	}
}

// TestXDSResolverWatchCallbackAfterClose tests the case where a service update
// from the underlying xdsClient is received after the resolver is closed.
func (s) TestXDSResolverWatchCallbackAfterClose(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)

	// Call the watchAPI callback after closing the resolver, and make sure no
	// update is triggerred on the ClientConn.
	xdsR.Close()
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes:  []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{cluster: {Weight: 1}}}},
			},
		},
	}, nil)

	sCtx, sCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	if gotVal, gotErr := tcc.stateCh.Receive(sCtx); gotErr != context.DeadlineExceeded {
		t.Fatalf("ClientConn.UpdateState called after xdsResolver is closed: %v", gotVal)
	}
}

// TestXDSResolverCloseClosesXDSClient tests that the XDS resolver's Close
// method closes the XDS client.
func (s) TestXDSResolverCloseClosesXDSClient(t *testing.T) {
	xdsR, _, _, cancel := testSetup(t, setupOpts{target: target})
	xdsR.Close()
	cancel() // Blocks until the xDS client is closed.
}

// TestXDSResolverBadServiceUpdate tests the case the xdsClient returns a bad
// service update.
func (s) TestXDSResolverBadServiceUpdate(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer xdsR.Close()
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)

	// Invoke the watchAPI callback with a bad service update and wait for the
	// ReportError method to be called on the ClientConn.
	suErr := errors.New("bad serviceupdate")
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{}, suErr)

	if gotErrVal, gotErr := tcc.errorCh.Receive(ctx); gotErr != nil || gotErrVal != suErr {
		t.Fatalf("ClientConn.ReportError() received %v, want %v", gotErrVal, suErr)
	}
}

// TestXDSResolverGoodServiceUpdate tests the happy case where the resolver
// gets a good service update from the xdsClient.
func (s) TestXDSResolverGoodServiceUpdate(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer xdsR.Close()
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)
	defer replaceRandNumGenerator(0)()

	for _, tt := range []struct {
		routes       []*xdsresource.Route
		wantJSON     string
		wantClusters map[string]bool
	}{
		{
			routes: []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{"test-cluster-1": {Weight: 1}}}},
			wantJSON: `{"loadBalancingConfig":[{
	 "xds_cluster_manager_experimental":{
	   "children":{
		 "cluster:test-cluster-1":{
		   "childPolicy":[{"cds_experimental":{"cluster":"test-cluster-1"}}]
		 }
	   }
	 }}]}`,
			wantClusters: map[string]bool{"cluster:test-cluster-1": true},
		},
		{
			routes: []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{
				"cluster_1": {Weight: 75},
				"cluster_2": {Weight: 25},
			}}},
			// This update contains the cluster from the previous update as
			// well as this update, as the previous config selector still
			// references the old cluster when the new one is pushed.
			wantJSON: `{"loadBalancingConfig":[{
	 "xds_cluster_manager_experimental":{
	   "children":{
		 "cluster:test-cluster-1":{
		   "childPolicy":[{"cds_experimental":{"cluster":"test-cluster-1"}}]
		 },
		 "cluster:cluster_1":{
		   "childPolicy":[{"cds_experimental":{"cluster":"cluster_1"}}]
		 },
		 "cluster:cluster_2":{
		   "childPolicy":[{"cds_experimental":{"cluster":"cluster_2"}}]
		 }
	   }
	 }}]}`,
			wantClusters: map[string]bool{"cluster:cluster_1": true, "cluster:cluster_2": true},
		},
		{
			routes: []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{
				"cluster_1": {Weight: 75},
				"cluster_2": {Weight: 25},
			}}},
			// With this redundant update, the old config selector has been
			// stopped, so there are no more references to the first cluster.
			// Only the second update's clusters should remain.
			wantJSON: `{"loadBalancingConfig":[{
	 "xds_cluster_manager_experimental":{
	   "children":{
		 "cluster:cluster_1":{
		   "childPolicy":[{"cds_experimental":{"cluster":"cluster_1"}}]
		 },
		 "cluster:cluster_2":{
		   "childPolicy":[{"cds_experimental":{"cluster":"cluster_2"}}]
		 }
	   }
	 }}]}`,
			wantClusters: map[string]bool{"cluster:cluster_1": true, "cluster:cluster_2": true},
		},
	} {
		// Invoke the watchAPI callback with a good service update and wait for the
		// UpdateState method to be called on the ClientConn.
		xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
			VirtualHosts: []*xdsresource.VirtualHost{
				{
					Domains: []string{targetStr},
					Routes:  tt.routes,
				},
			},
		}, nil)

		ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
		defer cancel()
		gotState, err := tcc.stateCh.Receive(ctx)
		if err != nil {
			t.Fatalf("Error waiting for UpdateState to be called: %v", err)
		}
		rState := gotState.(resolver.State)
		if err := rState.ServiceConfig.Err; err != nil {
			t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
		}

		wantSCParsed := internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(tt.wantJSON)
		if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
			t.Errorf("ClientConn.UpdateState received different service config")
			t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
			t.Error("want: ", cmp.Diff(nil, wantSCParsed.Config))
		}

		cs := iresolver.GetConfigSelector(rState)
		if cs == nil {
			t.Error("received nil config selector")
			continue
		}

		pickedClusters := make(map[string]bool)
		// Odds of picking 75% cluster 100 times in a row: 1 in 3E-13.  And
		// with the random number generator stubbed out, we can rely on this
		// to be 100% reproducible.
		for i := 0; i < 100; i++ {
			res, err := cs.SelectConfig(iresolver.RPCInfo{Context: context.Background()})
			if err != nil {
				t.Fatalf("Unexpected error from cs.SelectConfig(_): %v", err)
			}
			cluster := clustermanager.GetPickedClusterForTesting(res.Context)
			pickedClusters[cluster] = true
			res.OnCommitted()
		}
		if !reflect.DeepEqual(pickedClusters, tt.wantClusters) {
			t.Errorf("Picked clusters: %v; want: %v", pickedClusters, tt.wantClusters)
		}
	}
}

// TestResolverRequestHash tests a case where a resolver receives a RouteConfig update
// with a HashPolicy specifying to generate a hash. The configSelector generated should
// successfully generate a Hash.
func (s) TestResolverRequestHash(t *testing.T) {
	oldRH := envconfig.XDSRingHash
	envconfig.XDSRingHash = true
	defer func() { envconfig.XDSRingHash = oldRH }()

	mgmtServer, err := e2e.StartManagementServer(e2e.ManagementServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgmtServer.Stop()

	// Create a bootstrap configuration specifying the above management server.
	nodeID := uuid.New().String()
	cleanup, err := xdsbootstrap.CreateFile(xdsbootstrap.Options{
		NodeID:    nodeID,
		ServerURI: mgmtServer.Address,
		Version:   xdsbootstrap.TransportV3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	const serviceName = "my-service-client-side-xds"
	tcc, rClose := buildResolverForTarget(t, resolver.Target{URL: *testutils.MustParseURL("xds:///" + serviceName)})
	defer rClose()

	ldsName := serviceName
	rdsName := "route-" + serviceName
	// Configure the management server with a good listener resource and a
	// route configuration resource that specifies a hash policy.
	resources := e2e.UpdateOptions{
		NodeID:    nodeID,
		Listeners: []*v3listenerpb.Listener{e2e.DefaultClientListener(ldsName, rdsName)},
		Routes: []*v3routepb.RouteConfiguration{{
			Name: rdsName,
			VirtualHosts: []*v3routepb.VirtualHost{{
				Domains: []string{ldsName},
				Routes: []*v3routepb.Route{{
					Match: &v3routepb.RouteMatch{PathSpecifier: &v3routepb.RouteMatch_Prefix{Prefix: "/"}},
					Action: &v3routepb.Route_Route{Route: &v3routepb.RouteAction{
						ClusterSpecifier: &v3routepb.RouteAction_WeightedClusters{WeightedClusters: &v3routepb.WeightedCluster{
							Clusters: []*v3routepb.WeightedCluster_ClusterWeight{
								{
									Name:   "test-cluster-1",
									Weight: &wrapperspb.UInt32Value{Value: 100},
								},
							},
						}},
						HashPolicy: []*v3routepb.RouteAction_HashPolicy{{
							PolicySpecifier: &v3routepb.RouteAction_HashPolicy_Header_{
								Header: &v3routepb.RouteAction_HashPolicy_Header{
									HeaderName: ":path",
								},
							},
							Terminal: true,
						}},
					}},
				}},
			}},
		}},
		SkipValidation: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Read the update pushed by the resolver to the ClientConn.
	val, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Timeout waiting for an update from the resolver: %v", err)
	}
	rState := val.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("Received error in service config: %v", rState.ServiceConfig.Err)
	}
	cs := iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatal("Received nil config selector in update from resolver")
	}

	// Selecting a config when there was a hash policy specified in the route
	// that will be selected should put a request hash in the config's context.
	res, err := cs.SelectConfig(iresolver.RPCInfo{
		Context: metadata.NewOutgoingContext(ctx, metadata.Pairs(":path", "/products")),
		Method:  "/service/method",
	})
	if err != nil {
		t.Fatalf("cs.SelectConfig(): %v", err)
	}
	gotHash := ringhash.GetRequestHashForTesting(res.Context)
	wantHash := xxhash.Sum64String("/products")
	if gotHash != wantHash {
		t.Fatalf("Got request hash: %v, want: %v", gotHash, wantHash)
	}
}

// TestResolverRemovedWithRPCs tests the case where resources are removed from
// the management server, causing it to send an empty update to the xDS client,
// which returns a resource-not-found error to the xDS resolver. The test
// verifies that an ongoing RPC is handled properly when this happens.
func (s) TestResolverRemovedWithRPCs(t *testing.T) {
	mgmtServer, err := e2e.StartManagementServer(e2e.ManagementServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgmtServer.Stop()

	// Create a bootstrap configuration specifying the above management server.
	nodeID := uuid.New().String()
	cleanup, err := xdsbootstrap.CreateFile(xdsbootstrap.Options{
		NodeID:    nodeID,
		ServerURI: mgmtServer.Address,
		Version:   xdsbootstrap.TransportV3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	const serviceName = "my-service-client-side-xds"
	tcc, rClose := buildResolverForTarget(t, resolver.Target{URL: *testutils.MustParseURL("xds:///" + serviceName)})
	defer rClose()

	ldsName := serviceName
	rdsName := "route-" + serviceName
	// Configure the management server with a good listener and route
	// configuration resource.
	resources := e2e.UpdateOptions{
		NodeID:    nodeID,
		Listeners: []*v3listenerpb.Listener{e2e.DefaultClientListener(ldsName, rdsName)},
		Routes: []*v3routepb.RouteConfiguration{{
			Name: rdsName,
			VirtualHosts: []*v3routepb.VirtualHost{{
				Domains: []string{ldsName},
				Routes: []*v3routepb.Route{{
					Match: &v3routepb.RouteMatch{PathSpecifier: &v3routepb.RouteMatch_Prefix{Prefix: "/"}},
					Action: &v3routepb.Route_Route{Route: &v3routepb.RouteAction{
						ClusterSpecifier: &v3routepb.RouteAction_WeightedClusters{WeightedClusters: &v3routepb.WeightedCluster{
							Clusters: []*v3routepb.WeightedCluster_ClusterWeight{
								{
									Name:   "test-cluster-1",
									Weight: &wrapperspb.UInt32Value{Value: 100},
								},
							},
						}},
					}},
				}},
			}},
		}},
		SkipValidation: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Read the update pushed by the resolver to the ClientConn.
	val, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Timeout waiting for an update from the resolver: %v", err)
	}
	rState := val.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("Received error in service config: %v", rState.ServiceConfig.Err)
	}
	wantSCParsed := internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(`
{
	"loadBalancingConfig": [
		{
		  "xds_cluster_manager_experimental": {
			"children": {
			  "cluster:test-cluster-1": {
				"childPolicy": [
				  {
					"cds_experimental": {
					  "cluster": "test-cluster-1"
					} 
				  } 
				] 
			  } 
			} 
		  } 
		} 
	  ] 
}`)
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
		t.Errorf("Received unexpected service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Fatal("want: ", cmp.Diff(nil, wantSCParsed.Config))
	}

	cs := iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatal("Received nil config selector in update from resolver")
	}
	res, err := cs.SelectConfig(iresolver.RPCInfo{Context: ctx, Method: "/service/method"})
	if err != nil {
		t.Fatalf("cs.SelectConfig(): %v", err)
	}

	// Delete the resources on the management server. This should result in a
	// resource-not-found error from the xDS client.
	if err := mgmtServer.Update(ctx, e2e.UpdateOptions{NodeID: nodeID}); err != nil {
		t.Fatal(err)
	}

	// The RPC started earlier is still in progress. So, the xDS resolver will
	// not produce an empty service config at this point. Instead it will retain
	// the cluster to which the RPC is ongoing in the service config, but will
	// return an erroring config selector which will fail new RPCs.
	val, err = tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Timeout waiting for an update from the resolver: %v", err)
	}
	rState = val.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("Received error in service config: %v", rState.ServiceConfig.Err)
	}
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
		t.Errorf("Received unexpected service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Fatal("want: ", cmp.Diff(nil, wantSCParsed.Config))
	}
	cs = iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatal("Received nil config selector in update from resolver")
	}
	_, err = cs.SelectConfig(iresolver.RPCInfo{Context: ctx, Method: "/service/method"})
	if err == nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("cs.SelectConfig() returned: %v, want: %v", err, codes.Unavailable)
	}

	// "Finish the RPC"; this could cause a panic if the resolver doesn't
	// handle it correctly.
	res.OnCommitted()

	// Now that the RPC is committed, the xDS resolver is expected to send an
	// update with an empty service config.
	val, err = tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Timeout waiting for an update from the resolver: %v", err)
	}
	rState = val.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("Received error in service config: %v", rState.ServiceConfig.Err)
	}
	wantSCParsed = internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(`{}`)
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
		t.Errorf("Received unexpected service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Fatal("want: ", cmp.Diff(nil, wantSCParsed.Config))
	}
}

// TestXDSResolverRemovedResource tests for proper behavior after a resource is
// removed.
func (s) TestXDSResolverRemovedResource(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer cancel()
	defer xdsR.Close()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)

	// Invoke the watchAPI callback with a good service update and wait for the
	// UpdateState method to be called on the ClientConn.
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes:  []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{"test-cluster-1": {Weight: 1}}}},
			},
		},
	}, nil)
	wantJSON := `{"loadBalancingConfig":[{
	 "xds_cluster_manager_experimental":{
	   "children":{
		 "cluster:test-cluster-1":{
		   "childPolicy":[{"cds_experimental":{"cluster":"test-cluster-1"}}]
		 }
	   }
	 }}]}`
	wantSCParsed := internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(wantJSON)

	gotState, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState := gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
		t.Errorf("ClientConn.UpdateState received different service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Error("want: ", cmp.Diff(nil, wantSCParsed.Config))
	}

	// "Make an RPC" by invoking the config selector.
	cs := iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatalf("received nil config selector")
	}

	res, err := cs.SelectConfig(iresolver.RPCInfo{Context: context.Background()})
	if err != nil {
		t.Fatalf("Unexpected error from cs.SelectConfig(_): %v", err)
	}

	// "Finish the RPC"; this could cause a panic if the resolver doesn't
	// handle it correctly.
	res.OnCommitted()

	// Delete the resource.  The channel should receive a service config with the
	// original cluster but with an erroring config selector.
	suErr := xdsresource.NewErrorf(xdsresource.ErrorTypeResourceNotFound, "resource removed error")
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{}, suErr)

	if gotState, err = tcc.stateCh.Receive(ctx); err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState = gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
		t.Errorf("ClientConn.UpdateState received different service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Error("want: ", cmp.Diff(nil, wantSCParsed.Config))
	}

	// "Make another RPC" by invoking the config selector.
	cs = iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatalf("received nil config selector")
	}

	res, err = cs.SelectConfig(iresolver.RPCInfo{Context: context.Background()})
	if err == nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("Expected UNAVAILABLE error from cs.SelectConfig(_); got %v, %v", res, err)
	}

	// In the meantime, an empty ServiceConfig update should have been sent.
	if gotState, err = tcc.stateCh.Receive(ctx); err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState = gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}
	wantSCParsed = internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)("{}")
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
		t.Errorf("ClientConn.UpdateState received different service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Error("want: ", cmp.Diff(nil, wantSCParsed.Config))
	}
}

// TestResolverWRR tests the case where the route configuration returned by the
// management server contains a set of weighted clusters. The test performs a
// bunch of RPCs using the cluster specifier returned by the resolver, and
// verifies the cluster distribution.
func (s) TestResolverWRR(t *testing.T) {
	defer func(oldNewWRR func() wrr.WRR) { newWRR = oldNewWRR }(newWRR)
	newWRR = testutils.NewTestWRR

	mgmtServer, err := e2e.StartManagementServer(e2e.ManagementServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgmtServer.Stop()

	// Create a bootstrap configuration specifying the above management server.
	nodeID := uuid.New().String()
	cleanup, err := xdsbootstrap.CreateFile(xdsbootstrap.Options{
		NodeID:    nodeID,
		ServerURI: mgmtServer.Address,
		Version:   xdsbootstrap.TransportV3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	const serviceName = "my-service-client-side-xds"
	tcc, rClose := buildResolverForTarget(t, resolver.Target{URL: *testutils.MustParseURL("xds:///" + serviceName)})
	defer rClose()

	ldsName := serviceName
	rdsName := "route-" + serviceName
	// Configure the management server with a good listener resource and a
	// route configuration resource.
	resources := e2e.UpdateOptions{
		NodeID:    nodeID,
		Listeners: []*v3listenerpb.Listener{e2e.DefaultClientListener(ldsName, rdsName)},
		Routes: []*v3routepb.RouteConfiguration{{
			Name: rdsName,
			VirtualHosts: []*v3routepb.VirtualHost{{
				Domains: []string{ldsName},
				Routes: []*v3routepb.Route{{
					Match: &v3routepb.RouteMatch{PathSpecifier: &v3routepb.RouteMatch_Prefix{Prefix: "/"}},
					Action: &v3routepb.Route_Route{Route: &v3routepb.RouteAction{
						ClusterSpecifier: &v3routepb.RouteAction_WeightedClusters{WeightedClusters: &v3routepb.WeightedCluster{
							Clusters: []*v3routepb.WeightedCluster_ClusterWeight{
								{
									Name:   "A",
									Weight: &wrapperspb.UInt32Value{Value: 75},
								},
								{
									Name:   "B",
									Weight: &wrapperspb.UInt32Value{Value: 25},
								},
							},
						}},
					}},
				}},
			}},
		}},
		SkipValidation: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Read the update pushed by the resolver to the ClientConn.
	gotState, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Timeout waiting for an update from the resolver: %v", err)
	}
	rState := gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("Received error in service config: %v", rState.ServiceConfig.Err)
	}
	cs := iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatal("Received nil config selector in update from resolver")
	}

	// Make RPCs are verify WRR behavior in the cluster specifier.
	picks := map[string]int{}
	for i := 0; i < 100; i++ {
		res, err := cs.SelectConfig(iresolver.RPCInfo{Context: ctx, Method: "/service/method"})
		if err != nil {
			t.Fatalf("cs.SelectConfig(): %v", err)
		}
		picks[clustermanager.GetPickedClusterForTesting(res.Context)]++
		res.OnCommitted()
	}
	want := map[string]int{"cluster:A": 75, "cluster:B": 25}
	if !cmp.Equal(picks, want) {
		t.Errorf("Picked clusters: %v; want: %v", picks, want)
	}
}

func (s) TestXDSResolverMaxStreamDuration(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer xdsR.Close()
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, MaxStreamDuration: time.Second, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)

	defer func(oldNewWRR func() wrr.WRR) { newWRR = oldNewWRR }(newWRR)
	newWRR = testutils.NewTestWRR

	// Invoke the watchAPI callback with a good service update and wait for the
	// UpdateState method to be called on the ClientConn.
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes: []*xdsresource.Route{{
					Prefix:            newStringP("/foo"),
					WeightedClusters:  map[string]xdsresource.WeightedCluster{"A": {Weight: 1}},
					MaxStreamDuration: newDurationP(5 * time.Second),
				}, {
					Prefix:            newStringP("/bar"),
					WeightedClusters:  map[string]xdsresource.WeightedCluster{"B": {Weight: 1}},
					MaxStreamDuration: newDurationP(0),
				}, {
					Prefix:           newStringP(""),
					WeightedClusters: map[string]xdsresource.WeightedCluster{"C": {Weight: 1}},
				}},
			},
		},
	}, nil)

	gotState, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState := gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}

	cs := iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatal("received nil config selector")
	}

	testCases := []struct {
		name   string
		method string
		want   *time.Duration
	}{{
		name:   "RDS setting",
		method: "/foo/method",
		want:   newDurationP(5 * time.Second),
	}, {
		name:   "explicit zero in RDS; ignore LDS",
		method: "/bar/method",
		want:   nil,
	}, {
		name:   "no config in RDS; fallback to LDS",
		method: "/baz/method",
		want:   newDurationP(time.Second),
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := iresolver.RPCInfo{
				Method:  tc.method,
				Context: context.Background(),
			}
			res, err := cs.SelectConfig(req)
			if err != nil {
				t.Errorf("Unexpected error from cs.SelectConfig(%v): %v", req, err)
				return
			}
			res.OnCommitted()
			got := res.MethodConfig.Timeout
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("For method %q: res.MethodConfig.Timeout = %v; want %v", tc.method, got, tc.want)
			}
		})
	}
}

// TestXDSResolverDelayedOnCommitted tests that clusters remain in service
// config if RPCs are in flight.
func (s) TestXDSResolverDelayedOnCommitted(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer xdsR.Close()
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)

	// Invoke the watchAPI callback with a good service update and wait for the
	// UpdateState method to be called on the ClientConn.
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes:  []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{"test-cluster-1": {Weight: 1}}}},
			},
		},
	}, nil)

	gotState, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState := gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}

	wantJSON := `{"loadBalancingConfig":[{
	 "xds_cluster_manager_experimental":{
	   "children":{
		 "cluster:test-cluster-1":{
		   "childPolicy":[{"cds_experimental":{"cluster":"test-cluster-1"}}]
		 }
	   }
	 }}]}`
	wantSCParsed := internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(wantJSON)
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed.Config) {
		t.Errorf("ClientConn.UpdateState received different service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Fatal("want: ", cmp.Diff(nil, wantSCParsed.Config))
	}

	cs := iresolver.GetConfigSelector(rState)
	if cs == nil {
		t.Fatal("received nil config selector")
	}

	res, err := cs.SelectConfig(iresolver.RPCInfo{Context: context.Background()})
	if err != nil {
		t.Fatalf("Unexpected error from cs.SelectConfig(_): %v", err)
	}
	cluster := clustermanager.GetPickedClusterForTesting(res.Context)
	if cluster != "cluster:test-cluster-1" {
		t.Fatalf("")
	}
	// delay res.OnCommitted()

	// Perform TWO updates to ensure the old config selector does not hold a
	// reference to test-cluster-1.
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes:  []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{"NEW": {Weight: 1}}}},
			},
		},
	}, nil)
	tcc.stateCh.Receive(ctx) // Ignore the first update.

	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes:  []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{"NEW": {Weight: 1}}}},
			},
		},
	}, nil)

	gotState, err = tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState = gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}
	wantJSON2 := `{"loadBalancingConfig":[{
	 "xds_cluster_manager_experimental":{
	   "children":{
		 "cluster:test-cluster-1":{
		   "childPolicy":[{"cds_experimental":{"cluster":"test-cluster-1"}}]
		 },
		 "cluster:NEW":{
		   "childPolicy":[{"cds_experimental":{"cluster":"NEW"}}]
		 }
	   }
	 }}]}`
	wantSCParsed2 := internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(wantJSON2)
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed2.Config) {
		t.Errorf("ClientConn.UpdateState received different service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Fatal("want: ", cmp.Diff(nil, wantSCParsed2.Config))
	}

	// Invoke OnCommitted; should lead to a service config update that deletes
	// test-cluster-1.
	res.OnCommitted()

	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes:  []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{"NEW": {Weight: 1}}}},
			},
		},
	}, nil)
	gotState, err = tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState = gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}
	wantJSON3 := `{"loadBalancingConfig":[{
	 "xds_cluster_manager_experimental":{
	   "children":{
		 "cluster:NEW":{
		   "childPolicy":[{"cds_experimental":{"cluster":"NEW"}}]
		 }
	   }
	 }}]}`
	wantSCParsed3 := internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)(wantJSON3)
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantSCParsed3.Config) {
		t.Errorf("ClientConn.UpdateState received different service config")
		t.Error("got: ", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Fatal("want: ", cmp.Diff(nil, wantSCParsed3.Config))
	}
}

// TestXDSResolverUpdates tests the cases where the resolver gets a good update
// after an error, and an error after the good update.
func (s) TestXDSResolverGoodUpdateAfterError(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer xdsR.Close()
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)

	// Invoke the watchAPI callback with a bad service update and wait for the
	// ReportError method to be called on the ClientConn.
	suErr := errors.New("bad serviceupdate")
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{}, suErr)

	if gotErrVal, gotErr := tcc.errorCh.Receive(ctx); gotErr != nil || gotErrVal != suErr {
		t.Fatalf("ClientConn.ReportError() received %v, want %v", gotErrVal, suErr)
	}

	// Invoke the watchAPI callback with a good service update and wait for the
	// UpdateState method to be called on the ClientConn.
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
		VirtualHosts: []*xdsresource.VirtualHost{
			{
				Domains: []string{targetStr},
				Routes:  []*xdsresource.Route{{Prefix: newStringP(""), WeightedClusters: map[string]xdsresource.WeightedCluster{cluster: {Weight: 1}}}},
			},
		},
	}, nil)
	gotState, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState := gotState.(resolver.State)
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}

	// Invoke the watchAPI callback with a bad service update and wait for the
	// ReportError method to be called on the ClientConn.
	suErr2 := errors.New("bad serviceupdate 2")
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{}, suErr2)
	if gotErrVal, gotErr := tcc.errorCh.Receive(ctx); gotErr != nil || gotErrVal != suErr2 {
		t.Fatalf("ClientConn.ReportError() received %v, want %v", gotErrVal, suErr2)
	}
}

// TestXDSResolverResourceNotFoundError tests the cases where the resolver gets
// a ResourceNotFoundError. It should generate a service config picking
// weighted_target, but no child balancers.
func (s) TestXDSResolverResourceNotFoundError(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer xdsR.Close()
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)

	// Invoke the watchAPI callback with a bad service update and wait for the
	// ReportError method to be called on the ClientConn.
	suErr := xdsresource.NewErrorf(xdsresource.ErrorTypeResourceNotFound, "resource removed error")
	xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{}, suErr)

	sCtx, sCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	if gotErrVal, gotErr := tcc.errorCh.Receive(sCtx); gotErr != context.DeadlineExceeded {
		t.Fatalf("ClientConn.ReportError() received %v, %v, want channel recv timeout", gotErrVal, gotErr)
	}

	ctx, cancel = context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	gotState, err := tcc.stateCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for UpdateState to be called: %v", err)
	}
	rState := gotState.(resolver.State)
	wantParsedConfig := internal.ParseServiceConfig.(func(string) *serviceconfig.ParseResult)("{}")
	if !internal.EqualServiceConfigForTesting(rState.ServiceConfig.Config, wantParsedConfig.Config) {
		t.Error("ClientConn.UpdateState got wrong service config")
		t.Errorf("gotParsed: %s", cmp.Diff(nil, rState.ServiceConfig.Config))
		t.Errorf("wantParsed: %s", cmp.Diff(nil, wantParsedConfig.Config))
	}
	if err := rState.ServiceConfig.Err; err != nil {
		t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
	}
}

// TestXDSResolverMultipleLDSUpdates tests the case where two LDS updates with
// the same RDS name to watch are received without an RDS in between. Those LDS
// updates shouldn't trigger service config update.
//
// This test case also makes sure the resolver doesn't panic.
func (s) TestXDSResolverMultipleLDSUpdates(t *testing.T) {
	xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
	defer xdsR.Close()
	defer cancel()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	waitForWatchListener(ctx, t, xdsC, targetStr)
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	waitForWatchRouteConfig(ctx, t, xdsC, routeStr)
	defer replaceRandNumGenerator(0)()

	// Send a new LDS update, with the same fields.
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, HTTPFilters: routerFilterList}, nil)
	ctx, cancel = context.WithTimeout(context.Background(), defaultTestShortTimeout)
	defer cancel()
	// Should NOT trigger a state update.
	gotState, err := tcc.stateCh.Receive(ctx)
	if err == nil {
		t.Fatalf("ClientConn.UpdateState received %v, want timeout error", gotState)
	}

	// Send a new LDS update, with the same RDS name, but different fields.
	xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{RouteConfigName: routeStr, MaxStreamDuration: time.Second, HTTPFilters: routerFilterList}, nil)
	ctx, cancel = context.WithTimeout(context.Background(), defaultTestShortTimeout)
	defer cancel()
	gotState, err = tcc.stateCh.Receive(ctx)
	if err == nil {
		t.Fatalf("ClientConn.UpdateState received %v, want timeout error", gotState)
	}
}

type filterBuilder struct {
	httpfilter.Filter // embedded as we do not need to implement registry / parsing in this test.
	path              *[]string
}

var _ httpfilter.ClientInterceptorBuilder = &filterBuilder{}

func (fb *filterBuilder) BuildClientInterceptor(config, override httpfilter.FilterConfig) (iresolver.ClientInterceptor, error) {
	if config == nil {
		panic("unexpected missing config")
	}
	*fb.path = append(*fb.path, "build:"+config.(filterCfg).s)
	err := config.(filterCfg).newStreamErr
	if override != nil {
		*fb.path = append(*fb.path, "override:"+override.(filterCfg).s)
		err = override.(filterCfg).newStreamErr
	}

	return &filterInterceptor{path: fb.path, s: config.(filterCfg).s, err: err}, nil
}

type filterInterceptor struct {
	path *[]string
	s    string
	err  error
}

func (fi *filterInterceptor) NewStream(ctx context.Context, ri iresolver.RPCInfo, done func(), newStream func(ctx context.Context, done func()) (iresolver.ClientStream, error)) (iresolver.ClientStream, error) {
	*fi.path = append(*fi.path, "newstream:"+fi.s)
	if fi.err != nil {
		return nil, fi.err
	}
	d := func() {
		*fi.path = append(*fi.path, "done:"+fi.s)
		done()
	}
	cs, err := newStream(ctx, d)
	if err != nil {
		return nil, err
	}
	return &clientStream{ClientStream: cs, path: fi.path, s: fi.s}, nil
}

type clientStream struct {
	iresolver.ClientStream
	path *[]string
	s    string
}

type filterCfg struct {
	httpfilter.FilterConfig
	s            string
	newStreamErr error
}

func (s) TestXDSResolverHTTPFilters(t *testing.T) {
	var path []string
	testCases := []struct {
		name         string
		ldsFilters   []xdsresource.HTTPFilter
		vhOverrides  map[string]httpfilter.FilterConfig
		rtOverrides  map[string]httpfilter.FilterConfig
		clOverrides  map[string]httpfilter.FilterConfig
		rpcRes       map[string][][]string
		selectErr    string
		newStreamErr string
	}{
		{
			name: "no router filter",
			ldsFilters: []xdsresource.HTTPFilter{
				{Name: "foo", Filter: &filterBuilder{path: &path}, Config: filterCfg{s: "foo1"}},
			},
			rpcRes: map[string][][]string{
				"1": {
					{"build:foo1", "override:foo2", "build:bar1", "override:bar2", "newstream:foo1", "newstream:bar1", "done:bar1", "done:foo1"},
				},
			},
			selectErr: "no router filter present",
		},
		{
			name: "ignored after router filter",
			ldsFilters: []xdsresource.HTTPFilter{
				{Name: "foo", Filter: &filterBuilder{path: &path}, Config: filterCfg{s: "foo1"}},
				routerFilter,
				{Name: "foo2", Filter: &filterBuilder{path: &path}, Config: filterCfg{s: "foo2"}},
			},
			rpcRes: map[string][][]string{
				"1": {
					{"build:foo1", "newstream:foo1", "done:foo1"},
				},
				"2": {
					{"build:foo1", "newstream:foo1", "done:foo1"},
					{"build:foo1", "newstream:foo1", "done:foo1"},
					{"build:foo1", "newstream:foo1", "done:foo1"},
				},
			},
		},
		{
			name: "NewStream error; ensure earlier interceptor Done is still called",
			ldsFilters: []xdsresource.HTTPFilter{
				{Name: "foo", Filter: &filterBuilder{path: &path}, Config: filterCfg{s: "foo1"}},
				{Name: "bar", Filter: &filterBuilder{path: &path}, Config: filterCfg{s: "bar1", newStreamErr: errors.New("bar newstream err")}},
				routerFilter,
			},
			rpcRes: map[string][][]string{
				"1": {
					{"build:foo1", "build:bar1", "newstream:foo1", "newstream:bar1" /* <err in bar1 NewStream> */, "done:foo1"},
				},
				"2": {
					{"build:foo1", "build:bar1", "newstream:foo1", "newstream:bar1" /* <err in bar1 NewSteam> */, "done:foo1"},
				},
			},
			newStreamErr: "bar newstream err",
		},
		{
			name: "all overrides",
			ldsFilters: []xdsresource.HTTPFilter{
				{Name: "foo", Filter: &filterBuilder{path: &path}, Config: filterCfg{s: "foo1", newStreamErr: errors.New("this is overridden to nil")}},
				{Name: "bar", Filter: &filterBuilder{path: &path}, Config: filterCfg{s: "bar1"}},
				routerFilter,
			},
			vhOverrides: map[string]httpfilter.FilterConfig{"foo": filterCfg{s: "foo2"}, "bar": filterCfg{s: "bar2"}},
			rtOverrides: map[string]httpfilter.FilterConfig{"foo": filterCfg{s: "foo3"}, "bar": filterCfg{s: "bar3"}},
			clOverrides: map[string]httpfilter.FilterConfig{"foo": filterCfg{s: "foo4"}, "bar": filterCfg{s: "bar4"}},
			rpcRes: map[string][][]string{
				"1": {
					{"build:foo1", "override:foo2", "build:bar1", "override:bar2", "newstream:foo1", "newstream:bar1", "done:bar1", "done:foo1"},
					{"build:foo1", "override:foo2", "build:bar1", "override:bar2", "newstream:foo1", "newstream:bar1", "done:bar1", "done:foo1"},
				},
				"2": {
					{"build:foo1", "override:foo3", "build:bar1", "override:bar3", "newstream:foo1", "newstream:bar1", "done:bar1", "done:foo1"},
					{"build:foo1", "override:foo4", "build:bar1", "override:bar4", "newstream:foo1", "newstream:bar1", "done:bar1", "done:foo1"},
					{"build:foo1", "override:foo3", "build:bar1", "override:bar3", "newstream:foo1", "newstream:bar1", "done:bar1", "done:foo1"},
					{"build:foo1", "override:foo4", "build:bar1", "override:bar4", "newstream:foo1", "newstream:bar1", "done:bar1", "done:foo1"},
				},
			},
		},
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			xdsR, xdsC, tcc, cancel := testSetup(t, setupOpts{target: target})
			defer xdsR.Close()
			defer cancel()

			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()
			waitForWatchListener(ctx, t, xdsC, targetStr)

			xdsC.InvokeWatchListenerCallback(xdsresource.ListenerUpdate{
				RouteConfigName: routeStr,
				HTTPFilters:     tc.ldsFilters,
			}, nil)
			if i == 0 {
				waitForWatchRouteConfig(ctx, t, xdsC, routeStr)
			}

			defer func(oldNewWRR func() wrr.WRR) { newWRR = oldNewWRR }(newWRR)
			newWRR = testutils.NewTestWRR

			// Invoke the watchAPI callback with a good service update and wait for the
			// UpdateState method to be called on the ClientConn.
			xdsC.InvokeWatchRouteConfigCallback("", xdsresource.RouteConfigUpdate{
				VirtualHosts: []*xdsresource.VirtualHost{
					{
						Domains: []string{targetStr},
						Routes: []*xdsresource.Route{{
							Prefix: newStringP("1"), WeightedClusters: map[string]xdsresource.WeightedCluster{
								"A": {Weight: 1},
								"B": {Weight: 1},
							},
						}, {
							Prefix: newStringP("2"), WeightedClusters: map[string]xdsresource.WeightedCluster{
								"A": {Weight: 1},
								"B": {Weight: 1, HTTPFilterConfigOverride: tc.clOverrides},
							},
							HTTPFilterConfigOverride: tc.rtOverrides,
						}},
						HTTPFilterConfigOverride: tc.vhOverrides,
					},
				},
			}, nil)

			gotState, err := tcc.stateCh.Receive(ctx)
			if err != nil {
				t.Fatalf("Error waiting for UpdateState to be called: %v", err)
			}
			rState := gotState.(resolver.State)
			if err := rState.ServiceConfig.Err; err != nil {
				t.Fatalf("ClientConn.UpdateState received error in service config: %v", rState.ServiceConfig.Err)
			}

			cs := iresolver.GetConfigSelector(rState)
			if cs == nil {
				t.Fatal("received nil config selector")
			}

			for method, wants := range tc.rpcRes {
				// Order of wants is non-deterministic.
				remainingWant := make([][]string, len(wants))
				copy(remainingWant, wants)
				for n := range wants {
					path = nil

					res, err := cs.SelectConfig(iresolver.RPCInfo{Method: method, Context: context.Background()})
					if tc.selectErr != "" {
						if err == nil || !strings.Contains(err.Error(), tc.selectErr) {
							t.Errorf("SelectConfig(_) = _, %v; want _, Contains(%v)", err, tc.selectErr)
						}
						if err == nil {
							res.OnCommitted()
						}
						continue
					}
					if err != nil {
						t.Fatalf("Unexpected error from cs.SelectConfig(_): %v", err)
					}
					var doneFunc func()
					_, err = res.Interceptor.NewStream(context.Background(), iresolver.RPCInfo{}, func() {}, func(ctx context.Context, done func()) (iresolver.ClientStream, error) {
						doneFunc = done
						return nil, nil
					})
					if tc.newStreamErr != "" {
						if err == nil || !strings.Contains(err.Error(), tc.newStreamErr) {
							t.Errorf("NewStream(...) = _, %v; want _, Contains(%v)", err, tc.newStreamErr)
						}
						if err == nil {
							res.OnCommitted()
							doneFunc()
						}
						continue
					}
					if err != nil {
						t.Fatalf("unexpected error from Interceptor.NewStream: %v", err)

					}
					res.OnCommitted()
					doneFunc()

					// Confirm the desired path is found in remainingWant, and remove it.
					pass := false
					for i := range remainingWant {
						if reflect.DeepEqual(path, remainingWant[i]) {
							remainingWant[i] = remainingWant[len(remainingWant)-1]
							remainingWant = remainingWant[:len(remainingWant)-1]
							pass = true
							break
						}
					}
					if !pass {
						t.Errorf("%q:%v - path:\n%v\nwant one of:\n%v", method, n, path, remainingWant)
					}
				}
			}
		})
	}
}

func replaceRandNumGenerator(start int64) func() {
	nextInt := start
	xdsresource.RandInt63n = func(int64) (ret int64) {
		ret = nextInt
		nextInt++
		return
	}
	return func() {
		xdsresource.RandInt63n = grpcrand.Int63n
	}
}

func newDurationP(d time.Duration) *time.Duration {
	return &d
}
