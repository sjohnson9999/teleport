/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	authztypes "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client/proto"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/api/utils/keys"
	"github.com/gravitational/teleport/entitlements"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/auth/authtest"
	"github.com/gravitational/teleport/lib/auth/keygen"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/cryptosuites"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/events/eventstest"
	"github.com/gravitational/teleport/lib/inventory"
	"github.com/gravitational/teleport/lib/kube/proxy/streamproto"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/multiplexer"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/reversetunnelclient"
	"github.com/gravitational/teleport/lib/services"
	sessPkg "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils/log/logtest"
)

type TestContext struct {
	HostID               string
	ClusterName          string
	TLSServer            *authtest.TLSServer
	AuthServer           *auth.Server
	AuthClient           *authclient.Client
	Authz                authz.Authorizer
	KubeServer           *TLSServer
	KubeProxy            *TLSServer
	Emitter              *eventstest.ChannelEmitter
	Context              context.Context
	kubeServerListener   net.Listener
	kubeProxyListener    net.Listener
	cancel               context.CancelFunc
	heartbeatCtx         context.Context
	heartbeatCancel      context.CancelFunc
	lockWatcher          *services.LockWatcher
	closeSessionTrackers chan struct{}
}

// KubeClusterConfig defines the cluster to be created
type KubeClusterConfig struct {
	// Name is the cluster name.
	Name string
	// APIEndpoint is the cluster API endpoint.
	APIEndpoint string
}

// TestConfig defines the suite options.
type TestConfig struct {
	Clusters             []KubeClusterConfig
	ResourceMatchers     []services.ResourceMatcher
	OnReconcile          func(types.KubeClusters)
	OnEvent              func(apievents.AuditEvent)
	ClusterFeatures      func() proto.Features
	CreateAuditStreamErr error
}

// SetupTestContext creates a kube service with clusters configured.
func SetupTestContext(ctx context.Context, t *testing.T, cfg TestConfig) *TestContext {
	ctx, cancel := context.WithCancel(ctx)
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	testCtx := &TestContext{
		ClusterName:          "root.example.com",
		HostID:               uuid.New().String(),
		Context:              ctx,
		cancel:               cancel,
		heartbeatCtx:         heartbeatCtx,
		heartbeatCancel:      heartbeatCancel,
		closeSessionTrackers: make(chan struct{}),
	}
	t.Cleanup(func() { testCtx.Close() })

	kubeConfigLocation := newKubeConfigFile(t, cfg.Clusters...)

	// Create and start test auth server.
	authServer, err := authtest.NewAuthServer(authtest.AuthServerConfig{
		Clock:       clockwork.NewFakeClockAt(time.Now()),
		ClusterName: testCtx.ClusterName,
		Dir:         t.TempDir(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, authServer.Close()) })

	testCtx.TLSServer, err = authServer.NewTestTLSServer(
		// This test context is used by a test that stalls the LockWatcher to
		// simulate the enforcement of the strict lock mode. When the test fakes
		// the stall, the LockWatcher will enter a loop that constantly tries to
		// pull locks from the backend to recover from the stall. This context causes
		// the LockWatcher to hit the connection rate limit and fail with an error
		// different from the expected one. We setup a custom rate limiter to avoid
		// this issue.
		authtest.WithLimiterConfig(
			&limiter.Config{
				MaxConnections: 100000,
			},
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testCtx.TLSServer.Close()) })

	testCtx.AuthServer = testCtx.TLSServer.Auth()

	// Use sync recording to not involve the uploader.
	recConfig, err := authServer.AuthServer.GetSessionRecordingConfig(ctx)
	require.NoError(t, err)
	// Always use *-sync to prevent fileStreamer from running against os.RemoveAll
	// once the test ends.
	recConfig.SetMode(types.RecordAtNodeSync)
	_, err = authServer.AuthServer.UpsertSessionRecordingConfig(ctx, recConfig)
	require.NoError(t, err)

	// Auth client for Kube service.
	testCtx.AuthClient, err = testCtx.TLSServer.NewClient(authtest.TestServerID(types.RoleKube, testCtx.HostID))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testCtx.AuthClient.Close()) })

	// Auth client, lock watcher and authorizer for Kube proxy.
	proxyAuthClient, err := testCtx.TLSServer.NewClient(authtest.TestBuiltin(types.RoleProxy))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, proxyAuthClient.Close()) })

	testCtx.lockWatcher, err = services.NewLockWatcher(ctx, services.LockWatcherConfig{
		ResourceWatcherConfig: services.ResourceWatcherConfig{
			Component: teleport.ComponentProxy,
			Client:    proxyAuthClient,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		testCtx.lockWatcher.Close()
	})
	testCtx.Authz, err = authz.NewAuthorizer(authz.AuthorizerOpts{
		ClusterName: testCtx.ClusterName,
		AccessPoint: proxyAuthClient,
		LockWatcher: testCtx.lockWatcher,
	})
	require.NoError(t, err)

	// TLS config for kube proxy and Kube service.
	serverIdentity, err := authtest.NewServerIdentity(authServer.AuthServer, testCtx.HostID, types.RoleKube)
	require.NoError(t, err)
	kubeServiceTLSConfig, err := serverIdentity.TLSConfig(nil)
	require.NoError(t, err)

	// Create test audit events emitter.
	testCtx.Emitter = eventstest.NewChannelEmitter(100)
	go func() {
		for {
			select {
			case evt := <-testCtx.Emitter.C():
				if cfg.OnEvent != nil {
					cfg.OnEvent(evt)
				}
			case <-testCtx.Context.Done():
				return
			}
		}
	}()
	keyGen := keygen.New(testCtx.Context)

	client := newAuthClientWithStreamer(testCtx, cfg.CreateAuditStreamErr)

	features := func() proto.Features {
		return proto.Features{
			Entitlements: map[string]*proto.EntitlementInfo{
				string(entitlements.K8s): {Enabled: true},
			},
		}
	}
	if cfg.ClusterFeatures != nil {
		features = cfg.ClusterFeatures
	}

	testCtx.kubeServerListener, err = net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	testCtx.kubeProxyListener, err = net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	inventoryHandle, err := inventory.NewDownstreamHandle(client.InventoryControlStream,
		func(_ context.Context) (*proto.UpstreamInventoryHello, error) {
			return &proto.UpstreamInventoryHello{
				ServerID: testCtx.HostID,
				Version:  teleport.Version,
				Services: types.SystemRoles{types.RoleKube}.StringSlice(),
				Hostname: "test",
			}, nil
		})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inventoryHandle.Close()) })

	// Create kubernetes service server.
	testCtx.KubeServer, err = NewTLSServer(TLSServerConfig{
		ForwarderConfig: ForwarderConfig{
			Namespace:   apidefaults.Namespace,
			Keygen:      keyGen,
			ClusterName: testCtx.ClusterName,
			Authz:       testCtx.Authz,
			// fileStreamer continues to write events after the server is shutdown and
			// races against os.RemoveAll leading the test to fail.
			// Using "node-sync" mode to write the events and session recordings
			// directly to AuthClient solves the issue.
			// We wrap the AuthClient with an events.TeeStreamer to send non-disk
			// events like session.end to testCtx.emitter as well.
			AuthClient: &fakeClient{ClientI: client, closeC: testCtx.closeSessionTrackers},
			// StreamEmitter is required although not used because we are using
			// "node-sync" as session recording mode.
			Emitter:           testCtx.Emitter,
			DataDir:           t.TempDir(),
			CachingAuthClient: client,
			HostID:            testCtx.HostID,
			Context:           testCtx.Context,
			KubeconfigPath:    kubeConfigLocation,
			KubeServiceType:   KubeService,
			Component:         teleport.ComponentKube,
			LockWatcher:       testCtx.lockWatcher,
			// skip Impersonation validation
			CheckImpersonationPermissions: func(ctx context.Context, clusterName string, sarClient authztypes.SelfSubjectAccessReviewInterface) error {
				return nil
			},
			Clock:           clockwork.NewRealClock(),
			ClusterFeatures: features,
		},
		DynamicLabels: nil,
		TLS:           kubeServiceTLSConfig.Clone(),
		AccessPoint:   client,
		LimiterConfig: limiter.Config{
			MaxConnections: 1000,
		},
		// each time heartbeat is called we insert data into the channel.
		// this is used to make sure that heartbeat started and the clusters
		// are registered in the auth server
		OnHeartbeat:          func(err error) {},
		GetRotation:          func(role types.SystemRole) (*types.Rotation, error) { return &types.Rotation{}, nil },
		ResourceMatchers:     cfg.ResourceMatchers,
		OnReconcile:          cfg.OnReconcile,
		Log:                  logtest.NewLogger(),
		InventoryHandle:      inventoryHandle,
		ConnectedProxyGetter: reversetunnel.NewConnectedProxyGetter(),
	})
	require.NoError(t, err)

	// Create kubernetes proxy server.
	kubeServersWatcher, err := services.NewKubeServerWatcher(
		testCtx.Context,
		services.KubeServerWatcherConfig{
			ResourceWatcherConfig: services.ResourceWatcherConfig{
				Component: teleport.ComponentKube,
				Client:    client,
			},
			KubernetesServerGetter: client,
		},
	)
	require.NoError(t, err)
	t.Cleanup(kubeServersWatcher.Close)

	// TLS config for kube proxy and Kube service.
	proxyServerIdentity, err := authtest.NewServerIdentity(authServer.AuthServer, testCtx.HostID, types.RoleProxy)
	require.NoError(t, err)
	proxyTLSConfig, err := proxyServerIdentity.TLSConfig(nil)
	require.NoError(t, err)
	require.Len(t, proxyTLSConfig.Certificates, 1)
	require.NotNil(t, proxyTLSConfig.RootCAs)

	// Create kubernetes service server.
	testCtx.KubeProxy, err = NewTLSServer(TLSServerConfig{
		ForwarderConfig: ForwarderConfig{
			ReverseTunnelSrv: &reversetunnelclient.FakeServer{
				Sites: []reversetunnelclient.RemoteSite{
					&fakeRemoteSite{
						FakeRemoteSite: reversetunnelclient.NewFakeRemoteSite(testCtx.ClusterName, client),
						idToAddr: map[string]string{
							testCtx.HostID: testCtx.kubeServerListener.Addr().String(),
						},
					},
				},
			},
			Namespace:   apidefaults.Namespace,
			Keygen:      keyGen,
			ClusterName: testCtx.ClusterName,
			Authz:       testCtx.Authz,
			// fileStreamer continues to write events after the server is shutdown and
			// races against os.RemoveAll leading the test to fail.
			// Using "node-sync" mode to write the events and session recordings
			// directly to AuthClient solves the issue.
			// We wrap the AuthClient with an events.TeeStreamer to send non-disk
			// events like session.end to testCtx.emitter as well.
			AuthClient: &fakeClient{ClientI: client, closeC: testCtx.closeSessionTrackers},
			// StreamEmitter is required although not used because we are using
			// "node-sync" as session recording mode.
			Emitter:           testCtx.Emitter,
			DataDir:           t.TempDir(),
			CachingAuthClient: client,
			HostID:            testCtx.HostID,
			Context:           testCtx.Context,
			KubeServiceType:   ProxyService,
			Component:         teleport.ComponentKube,
			LockWatcher:       testCtx.lockWatcher,
			Clock:             clockwork.NewRealClock(),
			ClusterFeatures:   features,
			GetConnTLSCertificate: func() (*tls.Certificate, error) {
				return &proxyTLSConfig.Certificates[0], nil
			},
			GetConnTLSRoots: func() (*x509.CertPool, error) {
				return proxyTLSConfig.RootCAs, nil
			},
			PROXYSigner: &multiplexer.PROXYSigner{},
		},
		TLS:                      proxyTLSConfig.Clone(),
		AccessPoint:              client,
		KubernetesServersWatcher: kubeServersWatcher,
		LimiterConfig: limiter.Config{
			MaxConnections: 1000,
		},
		Log:             logtest.NewLogger(),
		InventoryHandle: inventoryHandle,
		GetRotation: func(role types.SystemRole) (*types.Rotation, error) {
			return &types.Rotation{}, nil
		},
		ConnectedProxyGetter: reversetunnel.NewConnectedProxyGetter(),
	})
	require.NoError(t, err)
	require.Equal(t, testCtx.KubeServer.Server.ReadTimeout, time.Duration(0), "kube server write timeout must be 0")
	require.Equal(t, testCtx.KubeServer.Server.WriteTimeout, time.Duration(0), "kube server write timeout must be 0")

	testCtx.startKubeServices(t)
	// Explicitly send a heartbeat for any configured clusters.
	for _, cluster := range cfg.Clusters {
		select {
		case sender := <-inventoryHandle.Sender():
			server, err := testCtx.KubeServer.GetServerInfo(cluster.Name)
			require.NoError(t, err)
			require.NoError(t, sender.Send(ctx, &proto.InventoryHeartbeat{
				KubernetesServer: server,
			}))
		case <-time.After(20 * time.Second):
			t.Fatal("timed out waiting for inventory handle sender")
		}
	}

	// Wait for kube servers to be initialized.
	require.NoError(t, kubeServersWatcher.WaitInitialization())

	// Ensure watcher has the correct list of clusters.
	require.Eventually(t, func() bool {
		kubeServers, err := kubeServersWatcher.CurrentResources(ctx)
		return err == nil && len(kubeServers) == len(cfg.Clusters)
	}, 3*time.Second, time.Millisecond*100)

	return testCtx
}

// startKubeServices starts kube service and kube proxy to handle connections.
func (c *TestContext) startKubeServices(t *testing.T) {
	go func() {
		err := c.KubeServer.Serve(c.kubeServerListener)
		// ignore server closed error returned when .Close is called.
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		assert.NoError(t, err)
	}()

	go func() {
		err := c.KubeProxy.Serve(c.kubeProxyListener)
		// ignore server closed error returned when .Close is called.
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		assert.NoError(t, err)
	}()
}

// Close closes resources associated with the test context.
func (c *TestContext) Close() error {
	// cancel the heartbeat context to stop validating the heartbeat not found
	// errors when deprovisioning.
	c.heartbeatCancel()
	// kubeServer closes the listener
	errKubeServer := c.KubeServer.Close()
	errKubeProxy := c.KubeProxy.Close()
	authCErr := c.AuthClient.Close()
	authSErr := c.AuthServer.Close()
	c.cancel()
	return trace.NewAggregate(errKubeServer, errKubeProxy, authCErr, authSErr)
}

// KubeProxyAddress returns the address of the kube proxy.
func (c *TestContext) KubeProxyAddress() string {
	return c.kubeProxyListener.Addr().String()
}

// RoleSpec defiens the role name and kube details to be created.
type RoleSpec struct {
	Name           string
	KubeUsers      []string
	KubeGroups     []string
	SessionRequire []*types.SessionRequirePolicy
	SessionJoin    []*types.SessionJoinPolicy
	SetupRoleFunc  func(types.Role) // If nil all pods are allowed.
}

// CreateUserWithTraitsAndRole creates Teleport user and role with specified names
func (c *TestContext) CreateUserWithTraitsAndRole(ctx context.Context, t *testing.T, username string, userTraits map[string][]string, roleSpec RoleSpec) (types.User, types.Role) {
	return c.CreateUserWithTraitsAndRoleVersion(ctx, t, username, userTraits, types.DefaultRoleVersion, roleSpec)
}

func (c *TestContext) CreateUserWithTraitsAndRoleVersion(ctx context.Context, t *testing.T, username string, userTraits map[string][]string, roleVersion string, roleSpec RoleSpec) (types.User, types.Role) {
	user, role, err := authtest.CreateUserAndRole(
		c.TLSServer.Auth(),
		username,
		[]string{roleSpec.Name},
		nil,
		authtest.WithUserMutator(func(user types.User) {
			user.SetTraits(userTraits)
		}),
		authtest.WithRoleVersion(roleVersion),
	)
	require.NoError(t, err)
	role.SetKubeUsers(types.Allow, roleSpec.KubeUsers)
	role.SetKubeGroups(types.Allow, roleSpec.KubeGroups)
	role.SetSessionRequirePolicies(roleSpec.SessionRequire)
	role.SetSessionJoinPolicies(roleSpec.SessionJoin)

	if roleSpec.SetupRoleFunc == nil {
		role.SetKubeResources(types.Allow, []types.KubernetesResource{{Kind: "pods", Name: types.Wildcard, Namespace: types.Wildcard, Verbs: []string{types.Wildcard}, APIGroup: ""}})
	} else {
		roleSpec.SetupRoleFunc(role)
	}
	upsertedRole, err := c.TLSServer.Auth().UpsertRole(ctx, role)
	require.NoError(t, err)
	return user, upsertedRole
}

// CreateUserAndRole creates Teleport user and role with specified names
func (c *TestContext) CreateUserAndRole(ctx context.Context, t *testing.T, username string, roleSpec RoleSpec) (types.User, types.Role) {
	return c.CreateUserAndRoleVersion(ctx, t, username, types.DefaultRoleVersion, roleSpec)
}

// CreateUserAndRoleVersion creates Teleport user and role with specified names and role version.
func (c *TestContext) CreateUserAndRoleVersion(ctx context.Context, t *testing.T, username, roleVersion string, roleSpec RoleSpec) (types.User, types.Role) {
	return c.CreateUserWithTraitsAndRoleVersion(ctx, t, username, nil, roleVersion, roleSpec)
}

func newKubeConfigFile(t *testing.T, clusters ...KubeClusterConfig) string {
	tmpDir := t.TempDir()

	kubeConf := clientcmdapi.NewConfig()
	for _, cluster := range clusters {
		kubeConf.Clusters[cluster.Name] = &clientcmdapi.Cluster{
			Server:                cluster.APIEndpoint,
			InsecureSkipTLSVerify: true,
		}
		kubeConf.AuthInfos[cluster.Name] = &clientcmdapi.AuthInfo{}

		kubeConf.Contexts[cluster.Name] = &clientcmdapi.Context{
			Cluster:  cluster.Name,
			AuthInfo: cluster.Name,
		}
	}
	kubeConfigLocation := filepath.Join(tmpDir, "kubeconfig")
	err := clientcmd.WriteToFile(*kubeConf, kubeConfigLocation)
	require.NoError(t, err)
	return kubeConfigLocation
}

// GenTestKubeClientTLSCertOptions is a function that can be used to modify the
// identity used to generate the kube client certificate.
type GenTestKubeClientTLSCertOptions func(*tlsca.Identity)

// WithResourceAccessRequests adds resource access requests to the identity.
func WithResourceAccessRequests(r ...types.ResourceID) GenTestKubeClientTLSCertOptions {
	return func(identity *tlsca.Identity) {
		identity.AllowedResourceIDs = r
	}
}

// WithIdentityRoute allows the user to reset the identity's RouteToCluster
// and KubernetesCluster fields to empty strings. This is useful when the user
// wants to test path routing.
func WithIdentityRoute(routeToCluster, kubernetesCluster string) GenTestKubeClientTLSCertOptions {
	return func(identity *tlsca.Identity) {
		identity.RouteToCluster = routeToCluster
		identity.KubernetesCluster = kubernetesCluster
	}
}

// WithMFAVerified sets the MFAVerified identity field,
func WithMFAVerified() GenTestKubeClientTLSCertOptions {
	return func(i *tlsca.Identity) {
		i.MFAVerified = "fake"
	}
}

// GenTestKubeClientTLSCert generates a kube client to access kube service
func (c *TestContext) GenTestKubeClientTLSCert(t *testing.T, userName, kubeCluster string, opts ...GenTestKubeClientTLSCertOptions) (*kubernetes.Clientset, *rest.Config) {
	client, _, cfg := c.GenTestKubeClientsTLSCert(t, userName, kubeCluster, opts...)
	return client, cfg
}

// GenTestKubeClientsTLSCert generates a "regular" kube client and a dynamic one to access kube service
func (c *TestContext) GenTestKubeClientsTLSCert(t *testing.T, userName, kubeCluster string, opts ...GenTestKubeClientTLSCertOptions) (*kubernetes.Clientset, *dynamic.DynamicClient, *rest.Config) {
	authServer := c.AuthServer
	clusterName, err := authServer.GetClusterName(context.TODO())
	require.NoError(t, err)

	// Fetch user info to get roles and max session TTL.
	user, err := authServer.GetUser(context.Background(), userName, false)
	require.NoError(t, err)

	roles, err := services.FetchRoles(user.GetRoles(), authServer, user.GetTraits())
	require.NoError(t, err)

	ttl := roles.AdjustSessionTTL(10 * time.Minute)

	ca, err := authServer.GetCertAuthority(c.Context, types.CertAuthID{
		Type:       types.HostCA,
		DomainName: clusterName.GetClusterName(),
	}, true)
	require.NoError(t, err)

	caCert, signer, err := authServer.GetKeyStore().GetTLSCertAndSigner(c.Context, ca)
	require.NoError(t, err)

	tlsCA, err := tlsca.FromCertAndSigner(caCert, signer)
	require.NoError(t, err)

	priv, err := cryptosuites.GenerateKey(context.Background(),
		cryptosuites.GetCurrentSuiteFromAuthPreference(authServer),
		cryptosuites.UserTLS)
	require.NoError(t, err)
	// Sanity check we generated an ECDSA key.
	require.IsType(t, &ecdsa.PrivateKey{}, priv)
	privPEM, err := keys.MarshalPrivateKey(priv)
	require.NoError(t, err)

	id := tlsca.Identity{
		Username:          user.GetName(),
		Groups:            user.GetRoles(),
		KubernetesUsers:   user.GetKubeUsers(),
		KubernetesGroups:  user.GetKubeGroups(),
		KubernetesCluster: kubeCluster,
		RouteToCluster:    c.ClusterName,
		Traits:            user.GetTraits(),
	}
	for _, opt := range opts {
		opt(&id)
	}
	subj, err := id.Subject()
	require.NoError(t, err)

	cert, err := tlsCA.GenerateCertificate(tlsca.CertificateRequest{
		Clock:     authServer.GetClock(),
		PublicKey: priv.Public(),
		Subject:   subj,
		NotAfter:  authServer.GetClock().Now().Add(ttl),
	})
	require.NoError(t, err)

	tlsClientConfig := rest.TLSClientConfig{
		CAData:     ca.GetActiveKeys().TLS[0].Cert,
		CertData:   cert,
		KeyData:    privPEM,
		ServerName: "teleport.cluster.local",
	}
	restConfig := &rest.Config{
		Host:            "https://" + c.KubeProxyAddress(),
		TLSClientConfig: tlsClientConfig,
	}

	client, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)

	dynClient, err := dynamic.NewForConfig(restConfig)
	require.NoError(t, err)

	return client, dynClient, restConfig
}

// NewJoiningSession creates a new session stream for joining an existing session.
func (c *TestContext) NewJoiningSession(cfg *rest.Config, sessionID string, mode types.SessionParticipantMode) (*streamproto.SessionStream, error) {
	ws, err := newWebSocketClient(cfg, http.MethodPost, &url.URL{
		Scheme: "wss",
		Host:   c.KubeProxyAddress(),
		Path:   "/api/v1/teleport/join/" + sessionID,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = ws.connectViaWebsocket()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	stream, err := streamproto.NewSessionStream(ws.conn, streamproto.ClientHandshake{Mode: mode})
	return stream, trace.Wrap(err)
}

// authClientWithStreamer wraps auth.Client and replaces the CreateAuditStream
// and ResumeAuditStream methods to use a events.TeeStreamer to leverage the StreamEmitter
// even when recording mode is *-sync.
type authClientWithStreamer struct {
	*authclient.Client
	streamer             events.Streamer
	createAuditStreamErr error
}

// newAuthClientWithStreamer creates a new authClient wrapper.
func newAuthClientWithStreamer(testCtx *TestContext, createAuditStreamErr error) *authClientWithStreamer {
	return &authClientWithStreamer{Client: testCtx.AuthClient, streamer: testCtx.AuthClient, createAuditStreamErr: createAuditStreamErr}
}

func (a *authClientWithStreamer) CreateAuditStream(ctx context.Context, sID sessPkg.ID) (apievents.Stream, error) {
	if a.createAuditStreamErr != nil {
		return nil, trace.Wrap(a.createAuditStreamErr)
	}
	return a.streamer.CreateAuditStream(ctx, sID)
}

func (a *authClientWithStreamer) ResumeAuditStream(ctx context.Context, sID sessPkg.ID, uploadID string) (apievents.Stream, error) {
	return a.streamer.ResumeAuditStream(ctx, sID, uploadID)
}

type fakeClient struct {
	authclient.ClientI
	closeC chan struct{}
}

func (f *fakeClient) CreateSessionTracker(ctx context.Context, st types.SessionTracker) (types.SessionTracker, error) {
	select {
	case <-f.closeC:
		return nil, trace.ConnectionProblem(nil, "closed")
	default:
		return f.ClientI.CreateSessionTracker(ctx, st)
	}
}

// fakeRemoteSite is a fake remote site that uses a map to map server IDs to
// addresses to simulate reverse tunneling.
type fakeRemoteSite struct {
	*reversetunnelclient.FakeRemoteSite
	idToAddr map[string]string
}

func (f *fakeRemoteSite) DialTCP(p reversetunnelclient.DialParams) (conn net.Conn, err error) {
	// The server ID is the first part of the address.
	addr, ok := f.idToAddr[strings.Split(p.ServerID, ".")[0]]
	if !ok {
		return nil, trace.NotFound("server %q not found", p.ServerID)
	}
	conn, err = net.Dial("tcp", addr)
	if err != nil {
		panic(err)
	}
	return conn, nil
}
