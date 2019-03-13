package agent

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/api"
	credAppRole "github.com/hashicorp/vault/builtin/credential/approle"
	"github.com/hashicorp/vault/command/agent/auth"
	agentapprole "github.com/hashicorp/vault/command/agent/auth/approle"
	"github.com/hashicorp/vault/command/agent/cache"
	"github.com/hashicorp/vault/command/agent/sink"
	"github.com/hashicorp/vault/command/agent/sink/file"
	"github.com/hashicorp/vault/command/agent/sink/inmem"
	"github.com/hashicorp/vault/helper/logging"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/vault"
)

type (
	testAuthHelper struct {
		authHandler          *auth.AuthHandler
		roleID, secretID     string
		roleFile, secretFile string
		cleanup              func()
	}
)

func newTestAuthHelper(ctx context.Context, client *api.Client, policyBody string, tokenTtl time.Duration) (*testAuthHelper, error) {
	logger := logging.NewVaultLogger(log.Trace)

	// Add an kv-admin policy
	if err := client.Sys().PutPolicy("test-autoauth", policyBody); err != nil {
		return nil, err
	}

	// Enable approle
	err := client.Sys().EnableAuthWithOptions("approle", &api.EnableAuthOptions{
		Type: "approle",
	})
	if err != nil {
		return nil, err
	}

	_, err = client.Logical().Write("auth/approle/role/test1", map[string]interface{}{
		"bind_secret_id": "true",
		"token_ttl":      tokenTtl.String(),
		"token_max_ttl":  "10s",
		"policies":       []string{"test-autoauth"},
	})
	if err != nil {
		return nil, err
	}

	resp, err := client.Logical().Write("auth/approle/role/test1/secret-id", nil)
	if err != nil {
		return nil, err
	}
	secretID1 := resp.Data["secret_id"].(string)

	resp, err = client.Logical().Read("auth/approle/role/test1/role-id")
	if err != nil {
		return nil, err
	}
	roleID1 := resp.Data["role_id"].(string)

	rolef, err := ioutil.TempFile("", "auth.role-id.test.")
	if err != nil {
		return nil, err
	}
	role := rolef.Name()
	rolef.Close() // WriteFile doesn't need it open

	secretf, err := ioutil.TempFile("", "auth.secret-id.test.")
	if err != nil {
		return nil, err
	}
	secret := secretf.Name()
	secretf.Close()

	am, err := agentapprole.NewApproleAuthMethod(&auth.AuthConfig{
		Logger:    logger.Named("auth.approle"),
		MountPath: "auth/approle",
		Config: map[string]interface{}{
			"role_id_file_path":                   role,
			"secret_id_file_path":                 secret,
			"remove_secret_id_file_after_reading": true,
		},
	})
	if err != nil {
		return nil, err
	}

	ahConfig := &auth.AuthHandlerConfig{
		Logger: logger.Named("auth.handler"),
		Client: client,
	}
	authHandler := auth.NewAuthHandler(ahConfig)
	go authHandler.Run(ctx, am)
	go func() {
		<-ctx.Done()
		<-authHandler.DoneCh
	}()

	return &testAuthHelper{
		authHandler: authHandler,
		roleFile:    role,
		roleID:      roleID1,
		secretFile:  secret,
		secretID:    secretID1,
		cleanup: func() {
			os.Remove(role)
			os.Remove(secret)
		},
	}, nil
}

func (tah *testAuthHelper) getToken(tokenFile string) (string, error) {
	if err := ioutil.WriteFile(tah.roleFile, []byte(tah.roleID), 0600); err != nil {
		return "", err
	}

	if err := ioutil.WriteFile(tah.secretFile, []byte(tah.secretID), 0600); err != nil {
		return "", err
	}

	timeout := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(timeout) {
			return "", fmt.Errorf("did not find a written token after timeout")
		}
		val, err := ioutil.ReadFile(tokenFile)
		if err == nil {
			if len(val) == 0 {
				return "", fmt.Errorf("written token was empty")
			}

			_, err = os.Stat(tah.secretFile)
			if err == nil {
				return "", fmt.Errorf("secret file exists but was supposed to be removed")
			}

			return string(val), nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func testhelperAutoAuth(t *testing.T, fileSinks []*sink.SinkConfig, tokenFile string, tokenTtl time.Duration) (cleanup func(), client *api.Client, agentAddr string) {
	logger := logging.NewVaultLogger(log.Trace)
	coreConfig := &vault.CoreConfig{
		DisableMlock: true,
		DisableCache: true,
		Logger:       log.NewNullLogger(),
		LogicalBackends: map[string]logical.Factory{
			"kv": vault.LeasedPassthroughBackendFactory,
		},
		CredentialBackends: map[string]logical.Factory{
			"approle": credAppRole.Factory,
		},
	}

	cluster := vault.NewTestCluster(t, coreConfig, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
	})

	cluster.Start()

	vault.TestWaitActive(t, cluster.Cores[0].Core)
	client = cluster.Cores[0].Client

	defer os.Setenv(api.EnvVaultAddress, os.Getenv(api.EnvVaultAddress))
	os.Setenv(api.EnvVaultAddress, client.Address())

	defer os.Setenv(api.EnvVaultCACert, os.Getenv(api.EnvVaultCACert))
	os.Setenv(api.EnvVaultCACert, fmt.Sprintf("%s/ca_cert.pem", cluster.TempDir))

	err := client.Sys().Mount("kv", &api.MountInput{
		Type: "kv",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a secret in the backend
	_, err = client.Logical().Write("kv/foo", map[string]interface{}{
		"value": "bar",
		"ttl":   "1h",
	})
	if err != nil {
		t.Fatal(err)
	}

	cacheLogger := logging.NewVaultLogger(hclog.Trace).Named("cache")
	ctx, cancelFunc := context.WithCancel(context.Background())
	timer := time.AfterFunc(30*time.Second, func() {
		cancelFunc()
	})

	// Create the API proxier
	apiProxy, err := cache.NewAPIProxy(&cache.APIProxyConfig{
		Client: client,
		Logger: cacheLogger.Named("apiproxy"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create the lease cache proxier and set its underlying proxier to
	// the API proxier.
	leaseCache, err := cache.NewLeaseCache(&cache.LeaseCacheConfig{
		Client:      client,
		BaseContext: ctx,
		Proxier:     apiProxy,
		Logger:      cacheLogger.Named("leasecache"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ss := sink.NewSinkServer(&sink.SinkServerConfig{
		Logger: logger.Named("sink.server"),
		Client: client,
	})

	inmemSinkConfig := &sink.SinkConfig{
		Logger: logger.Named("sink.inmem"),
	}

	inmemSink, err := inmem.New(inmemSinkConfig, leaseCache)
	if err != nil {
		t.Fatal(err)
	}
	inmemSinkConfig.Sink = inmemSink

	policyBody := `
path "/kv/*" {
	capabilities = ["sudo", "create", "read", "update", "delete", "list"]
}

path "/auth/token/create" {
	capabilities = ["create", "update"]
}
`
	testAuthHelper, err := newTestAuthHelper(ctx, client, policyBody, tokenTtl)

	sinks := append([]*sink.SinkConfig{inmemSinkConfig}, fileSinks...)
	go ss.Run(ctx, testAuthHelper.authHandler.OutputCh, sinks)

	// Check that no sink file exists
	_, err = os.Lstat(tokenFile)
	if err == nil {
		t.Fatal("expected err")
	}
	if !os.IsNotExist(err) {
		t.Fatal("expected notexist err")
	}

	token, err := testAuthHelper.getToken(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("auto-auth token: %q", token)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// Create a muxer and add paths relevant for the lease cache layer
	mux := cache.AgentMux(ctx, cacheLogger, leaseCache, inmemSink, nil)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		ErrorLog:          cacheLogger.StandardLogger(nil),
	}
	go server.Serve(listener)

	cleanup = func() {
		os.Remove(tokenFile)
		listener.Close()
		cancelFunc()
		<-ss.DoneCh
		testAuthHelper.cleanup()
		timer.Stop()
		cluster.Cleanup()
	}

	return cleanup, client, "http://" + listener.Addr().String()
}

func TestCache_UsingAutoAuthToken(t *testing.T) {
	logger := logging.NewVaultLogger(log.Trace)

	var out string
	{
		// We close and rm this file right away because we're just basically testing
		// permissions and finding a usable file name for the sink to use below.
		ouf, err := ioutil.TempFile("", "auth.tokensink.test.")
		if err != nil {
			t.Fatal(err)
		}
		out = ouf.Name()
		ouf.Close()
		os.Remove(out)
		t.Logf("output: %s", out)
	}
	config := &sink.SinkConfig{
		Logger: logger.Named("sink.file"),
		Config: map[string]interface{}{
			"path": out,
		},
	}
	fs, err := file.NewFileSink(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Sink = fs

	tokenTtl := 3 * time.Second
	cleanup, realClient, agentAddr := testhelperAutoAuth(t, []*sink.SinkConfig{config}, out, tokenTtl)
	defer cleanup()

	// Wait for listeners to come up
	time.Sleep(2 * time.Second)

	testClient, err := api.NewClient(api.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	if err := testClient.SetAddress(agentAddr); err != nil {
		t.Fatal(err)
	}

	// This block tests that no token on the client is detected by the agent
	// and the auto-auth token is used
	{
		// Empty the token in the client to ensure that auto-auth token is used
		testClient.SetToken("")

		resp, err := testClient.Logical().Read("auth/token/lookup-self")
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil {
			t.Fatalf("failed to use the auto-auth token to perform lookup-self")
		}
	}

	// This block tests lease creation caching using the auto-auth token.
	{
		resp, err := testClient.Logical().Read("kv/foo")
		if err != nil {
			t.Fatal(err)
		}

		origReqID := resp.RequestID

		resp, err = testClient.Logical().Read("kv/foo")
		if err != nil {
			t.Fatal(err)
		}

		// Sleep for a bit to allow renewer logic to kick in
		time.Sleep(20 * time.Millisecond)

		cacheReqID := resp.RequestID

		if origReqID != cacheReqID {
			t.Fatalf("request ID  mismatch, expected second request to be a cached response: %s != %s", origReqID, cacheReqID)
		}
	}

	// This block tests auth token creation caching (child, non-orphan tokens)
	// using the auto-auth token.
	{
		resp, err := testClient.Logical().Write("auth/token/create", nil)
		if err != nil {
			t.Fatal(err)
		}
		origReqID := resp.RequestID

		// Sleep for a bit to allow renewer logic to kick in
		time.Sleep(20 * time.Millisecond)

		resp, err = testClient.Logical().Write("auth/token/create", nil)
		if err != nil {
			t.Fatal(err)
		}
		cacheReqID := resp.RequestID

		if origReqID != cacheReqID {
			t.Fatalf("request ID mismatch, expected second request to be a cached response: %s != %s", origReqID, cacheReqID)
		}
	}

	// This blocks tests that despite being allowed to use auto-auth token, the
	// token on the request will be prioritized.
	{
		// Empty the token in the client to ensure that auto-auth token is used
		testClient.SetToken(realClient.Token())

		resp, err := testClient.Logical().Read("auth/token/lookup-self")
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil || resp.Data["id"] != realClient.Token() {
			t.Fatalf("failed to use the cluster client token to perform lookup-self")
		}
	}
}

func TestCache_ClientAutoAuth(t *testing.T) {
	logger := logging.NewVaultLogger(log.Trace)

	var out string
	{
		// We close and rm this file right away because we're just basically testing
		// permissions and finding a usable file name for the sink to use below.
		ouf, err := ioutil.TempFile("", "auth.tokensink.test.")
		if err != nil {
			t.Fatal(err)
		}
		out = ouf.Name()
		ouf.Close()
		os.Remove(out)
		t.Logf("output: %s", out)
	}
	config := &sink.SinkConfig{
		Logger: logger.Named("sink.file"),
		Config: map[string]interface{}{
			"path": out,
		},
	}
	fs, err := file.NewFileSink(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Sink = fs

	tokenTtl := 200 * time.Millisecond
	cleanup, _, agentAddr := testhelperAutoAuth(t, []*sink.SinkConfig{config}, out, tokenTtl)
	defer cleanup()

	// Wait for listeners to come up
	time.Sleep(2 * time.Second)

	clientConfig := api.DefaultConfig()
	clientConfig.AgentFileSinkPath = out
	testClient, err := api.NewClient(clientConfig)
	if err != nil {
		t.Fatal(err)
	}

	if err := testClient.SetAddress(agentAddr); err != nil {
		t.Fatal(err)
	}

	// This block tests token renewal and the client reading in file updates.
	{
		_, err := testClient.Logical().Read("kv/foo")
		if err != nil {
			t.Fatal(err)
		}

		time.Sleep(tokenTtl)

		_, err = testClient.Logical().Read("kv/foo")
		if err != nil {
			t.Fatal(err)
		}
	}

}
