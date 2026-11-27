// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration

package base

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ServeOnClusterGateway serves handler from the test runner on the gateway address of a QEMU
// cluster, which the nodes can reach, and returns the base URL of the server.
//
// It skips the test when the cluster was not provisioned by this runner, as then the runner holds
// no address the nodes are known to reach. The server is self-signed when useTLS is set.
func (talosSuite *TalosSuite) ServeOnClusterGateway(t *testing.T, handler http.Handler, useTLS bool) string {
	t.Helper()

	if talosSuite.Cluster == nil || talosSuite.Cluster.Provisioner() != ProvisionerQEMU {
		t.Skip("skipping test for non-qemu provisioner")
	}

	gatewayAddrs := talosSuite.Cluster.Info().Network.GatewayAddrs
	if len(gatewayAddrs) == 0 {
		t.Skip("skipping test, the cluster has no gateway address")
	}

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(gatewayAddrs[0].String(), "0"))
	if err != nil {
		t.Skipf("skipping test, the test runner can't listen on the cluster gateway: %v", err)
	}

	srv := httptest.NewUnstartedServer(handler)
	srv.Listener.Close() //nolint:errcheck
	srv.Listener = listener

	if useTLS {
		srv.StartTLS()
	} else {
		srv.Start()
	}

	t.Cleanup(srv.Close)

	return srv.URL
}
