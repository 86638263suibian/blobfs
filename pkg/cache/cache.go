package cache

import (
	"path/filepath"
	"sync"

	"golang.org/x/net/context"

	"github.com/tsileo/blobstash/pkg/backend/blobsfile"
	"github.com/tsileo/blobstash/pkg/client/blobstore"
	"github.com/tsileo/blobstash/pkg/client/clientutil"
	"github.com/tsileo/blobstash/pkg/config/pathutil"
	"github.com/tsileo/blobstash/pkg/vkv"
)

// TODO(tsileo): add Clean/Reset/Remove methods

type Cache struct {
	backend *blobsfile.BlobsFileBackend
	bs      *blobstore.BlobStore
	kv      *vkv.DB
	wg      sync.WaitGroup
	// TODO(tsileo): embed a kvstore too (but witouth sync/), may be make it optional?
}

func New(opts *clientutil.Opts, name string) *Cache {
	wg := sync.WaitGroup{}
	backend := blobsfile.New(filepath.Join(pathutil.VarDir(), name), 0, false, wg)
	kv, err := vkv.New(filepath.Join(pathutil.VarDir(), name, "vkv"))
	if err != nil {
		panic(err)
	}
	return &Cache{
		kv:      kv,
		bs:      blobstore.New(opts),
		backend: backend,
		wg:      wg,
	}
}

func (c *Cache) Close() error {
	c.backend.Close()
	return c.kv.Close()
}

func (c *Cache) Vkv() *vkv.DB {
	return c.kv
}

func (c *Cache) Client() *clientutil.Client {
	return c.bs.Client()
}

func (c *Cache) PutRemote(hash string, blob []byte) error {
	return c.bs.Put(hash, blob)
}

func (c *Cache) Put(hash string, blob []byte) error {
	return c.backend.Put(hash, blob)
}

func (c *Cache) StatRemote(hash string) (bool, error) {
	return c.bs.Stat(hash)
}

func (c *Cache) Stat(hash string) (bool, error) {
	exists, err := c.backend.Stat(hash)
	if err != nil {
		return false, err
	}
	if !exists {
		return c.bs.Stat(hash)
	}
	return exists, err
}

func (c *Cache) Get(ctx context.Context, hash string) ([]byte, error) {
	blob, err := c.backend.Get(hash)
	switch err {
	// If the blob is not found locally, try to fetch it from the remote blobstore
	case clientutil.ErrBlobNotFound:
		blob, err = c.bs.Get(ctx, hash)
		if err != nil {
			return nil, err
		}
		// Save the blob locally for future fetch
		if err := c.backend.Put(hash, blob); err != nil {
			return nil, err
		}
	case nil:
	default:
		return nil, err
	}
	return blob, nil
}

func (c *Cache) Sync(syncfunc func()) error {
	// TODO(tsileo): a way to sync a subtree to the remote blobstore `bs`
	// Passing a func may not be the optimal way, better to expose an Enumerate? maybe not even needed?
	return nil
}
