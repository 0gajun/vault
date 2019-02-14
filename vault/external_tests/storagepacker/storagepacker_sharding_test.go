package storagepacker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/golang/protobuf/ptypes"
	consulapi "github.com/hashicorp/consul/api"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/helper/logging"
	"github.com/hashicorp/vault/helper/storagepacker"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/plugin/pb"
	"github.com/hashicorp/vault/physical/consul"
	"github.com/hashicorp/vault/vault"
	dockertest "gopkg.in/ory-am/dockertest.v2"
)

func TestStoragePacker_Sharding(t *testing.T) {
	var token string
	addr := os.Getenv("CONSUL_HTTP_ADDR")
	if addr == "" {
		cid, connURL := consul.PrepareConsulTestContainer(t)
		if cid != "" {
			defer consul.CleanupConsulTestContainer(t, cid)
		}
		addr = connURL
		token = dockertest.ConsulACLMasterToken
	}

	conf := consulapi.DefaultConfig()
	conf.Address = addr
	conf.Token = token
	consulClient, err := consulapi.NewClient(conf)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	randPath := fmt.Sprintf("vault-%d/", time.Now().Unix())
	defer func() {
		consulClient.KV().DeleteTree(randPath, nil)
	}()

	logger := logging.NewVaultLogger(log.Trace)

	b, err := consul.NewConsulBackend(map[string]string{
		"address":      conf.Address,
		"path":         randPath,
		"max_parallel": "256",
		"token":        conf.Token,
	}, logger)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	randBytes := make([]byte, 100000, 100000)
	n, err := rand.Read(randBytes)
	if n != 100000 {
		t.Fatalf("expected 100k bytes, read %d", n)
	}
	if err != nil {
		t.Fatal(err)
	}
	randString := base64.StdEncoding.EncodeToString(randBytes)

	cluster := vault.NewTestCluster(t, &vault.CoreConfig{
		Physical: b,
	}, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
		NumCores:    1,
		Logger:      logger,
	})

	cluster.Start()
	defer cluster.Cleanup()

	core := cluster.Cores[0]
	vault.TestWaitActive(t, core.Core)
	ctx := context.Background()
	numEntries := 5000

	storage := logical.NewLogicalStorage(core.UnderlyingStorage)
	bucketStorageView := logical.NewStorageView(storage, "packer/buckets/")
	packer, err := storagepacker.NewStoragePackerV2(ctx, &storagepacker.Config{
		BucketStorageView: bucketStorageView,
		ConfigStorageView: logical.NewStorageView(storage, ""),
		Logger:            logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	protoSecret := &pb.Secret{
		InternalData: randString,
	}
	messageAsAny, err := ptypes.MarshalAny(protoSecret)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < numEntries; i++ {
		if err := packer.PutItem(ctx, &storagepacker.Item{
			ID:      fmt.Sprintf("%05d", i),
			Message: messageAsAny,
		}); err != nil {
			t.Fatal(err)
		}
	}

	/*
		buckets, err := logical.CollectKeys(ctx, bucketStorageView.SubView("v2/"))
		if err != nil {
			t.Fatal(err)
		}
		if len(buckets) == 256 {
			t.Fatalf("%d", len(buckets))
		}
		t.Log(len(buckets))
		t.Log(buckets)
	*/

	// Create a new packer, then start looking for expected values
	packer, err = storagepacker.NewStoragePackerV2(ctx, &storagepacker.Config{
		BucketStorageView: bucketStorageView,
		ConfigStorageView: logical.NewStorageView(storage, ""),
		Logger:            logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("created new packer, validating entries")
	for i := 0; i < numEntries; i++ {
		item, err := packer.GetItem(ctx, fmt.Sprintf("%05d", i))
		if err != nil {
			t.Fatal(err)
		}
		if item == nil {
			t.Fatal("nil item")
		}
	}
	t.Log("validation complete")
}
