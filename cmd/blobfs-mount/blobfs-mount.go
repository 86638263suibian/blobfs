package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tsileo/blobfs/pkg/cache"
	"github.com/tsileo/blobfs/pkg/pathutil"
	"github.com/tsileo/blobfs/pkg/root"
	"gopkg.in/yaml.v2"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fuseutil"
	"github.com/tsileo/blobstash/pkg/apps/app"
	"github.com/tsileo/blobstash/pkg/client/blobstore"
	"github.com/tsileo/blobstash/pkg/client/kvstore"
	"github.com/tsileo/blobstash/pkg/filetree/filetreeutil/meta"
	"github.com/tsileo/blobstash/pkg/filetree/reader/filereader"
	"github.com/tsileo/blobstash/pkg/filetree/writer"
	"github.com/tsileo/blobstash/pkg/vkv"
	"golang.org/x/net/context"
	"gopkg.in/inconshreveable/log15.v2"
)

// TODO(tsileo): use fs func for invalidating kernel cache
// TODO(tsileo): conditional request on the remote kvstore
// TODO(tsileo): improve sync, better locking, check that x minutes without activity before sync
// and only scan the hash needed
// TODO(tsileo): handle setattr, user, ctime/atime, mode check by user
// TODO(tsileo):
// - a prune command using the GC
// - a cache command download all the blobs needed for the FS
// - basic conflict handling, copy new files, and file.conflicted if conflicts
// - a -no-startup-sync flag for offline use?
// - a -cache mode

const (
	debugSuffix = ".blobfs_debug"
	maxInt      = int(^uint(0) >> 1)
)

var virtualXAttrs = map[string]func(*meta.Meta) []byte{
	"ref": func(m *meta.Meta) []byte {
		return []byte(m.Hash)
	},
	"url": nil, // Will be computed dynamically
	// "last_sync": func(_ *meta.Meta) []byte {
	// 	stats.Lock()
	// 	defer stats.Unlock()
	// 	// TODO(tsileo): implement the lat_sync
	// 	return []byte("")
	// },
}

var (
	wg  sync.WaitGroup
	bfs *FS
)

var Usage = func() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "  %s NAME MOUNTPOINT\n", os.Args[0])
	flag.PrintDefaults()
}

var Log = log15.New("logger", "blobfs")
var stats *Stats

func WriteJSON(w http.ResponseWriter, data interface{}) {
	js, err := json.Marshal(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(js)
}

type API struct {
}

func (api *API) Serve(socketPath string) error {
	http.HandleFunc("/ref", apiRefHandler)
	http.HandleFunc("/sync", apiSyncHandler)
	http.HandleFunc("/pull", apiPullHandler)
	http.HandleFunc("/debug", apiDebugHandler)
	// http.HandleFunc("/log", apiLogHandler)
	http.HandleFunc("/public", apiPublicHandler)
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		panic(err)
	}
	defer func() {
		l.Close()
		os.Remove(socketPath)
	}()
	if err := http.Serve(l, nil); err != nil {
		panic(err)
	}
	return nil
}

type NodeStatus struct {
	Type string
	Path string
	Ref  string
}

func apiRefHandler(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, map[string]string{"ref": bfs.Mount().node.Meta().Hash})
}

type CheckoutReq struct {
	Ref string `json:"ref"`
}

// func apiCheckoutHandler(w http.ResponseWriter, r *http.Request) {
// 	if r.Method != "POST" {
// 		http.Error(w, "POST request expected", http.StatusMethodNotAllowed)
// 		return
// 	}
// 	cr := &CheckoutReq{}
// 	if err := json.NewDecoder(r.Body).Decode(cr); err != nil {
// 		if err != nil {
// 			panic(err)
// 		}
// 	}
// 	var immutable bool
// 	// if cr.Ref == bfs.latest.ref {
// 	// 	immutable = true
// 	// }
// 	if err := bfs.setRoot(cr.Ref, immutable); err != nil {
// 		panic(err)
// 	}
// 	WriteJSON(w, cr)
// }

func apiDebugHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "GET request expected", http.StatusMethodNotAllowed)
		return
	}
	fsName := fmt.Sprintf(rootKeyFmt, bfs.Name())
	// Fetch and save all the known remote mutations
	remoteVersions, err := bfs.rkv.Versions(fsName, 0, -1, 0)
	if err != nil {
		panic(err)
	}
	localRemoteVersions, err := bfs.lkv.Versions(fsName, 0, -1, 0)
	if err != nil {
		panic(err)
	}
	localVersions, err := bfs.lkv.Versions(fmt.Sprintf(localRootKeyFmt, bfs.Name()), 0, -1, 0)
	if err != nil {
		panic(err)
	}
	WriteJSON(w, map[string]interface{}{
		"remote":       remoteVersions,
		"local-remote": localRemoteVersions,
		"local":        localVersions,
	})

}

func apiSyncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST request expected", http.StatusMethodNotAllowed)
		return
	}
	comment, err := ioutil.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}
	if err := bfs.Push(comment); err != nil {
		panic(err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func apiPullHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST request expected", http.StatusMethodNotAllowed)
		return
	}
	if err := bfs.Pull(); err != nil {
		panic(err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func apiPublicHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// FIXME(tsileo): lock the FS?
	out := map[string]*meta.Meta{}
	rootDir := bfs.Mount().node.(*Dir)
	if err := iterDir(rootDir, func(node Node) error {
		if node.Meta().IsPublic() {
			out[node.Meta().Hash] = node.Meta()
		}
		return nil
	}); err != nil {
		panic(err)
	}
	WriteJSON(w, out)
}

type CommitLog struct {
	T       string `json:"t"`
	Ref     string `json:"ref"`
	Comment string `json:"comment"`
	Current bool   `json:"current"`
}

// FIXME(tsileo): use the local or remote vkv store for this???
// func apiLogHandler(w http.ResponseWriter, r *http.Request) {
// 	if r.Method != "GET" {
// 		w.WriteHeader(http.StatusMethodNotAllowed)
// 		return
// 	}
// 	out := []*CommitLog{}

// 	versions, err := bfs.kvs.Versions(fmt.Sprintf(rootKeyFmt, bfs.name), 0, -1, 0)
// 	switch err {
// 	case kvstore.ErrKeyNotFound:
// 	case nil:
// 		for _, v := range versions.Versions {
// 			croot := &root.Root{}
// 			if err := json.Unmarshal(v.Data, croot); err != nil {
// 				panic(err)
// 			}
// 			cl := &CommitLog{
// 				T:       time.Unix(0, int64(v.Version)).Format(time.RFC3339),
// 				Ref:     croot.Ref,
// 				Comment: croot.Comment,
// 			}
// 			out = append(out, cl)
// 		}
// 	default:
// 		panic(err)
// 	}
// 	WriteJSON(w, out)
// }

// iterDir executes the given callback `cb` on each nodes (file or dir) recursively.
func iterDir(dir *Dir, cb func(n Node) error) error {
	if dir.Children == nil {
		if err := dir.reload(); err != nil {
			return err
		}
	}

	for _, node := range dir.Children {
		if node.IsDir() {
			if err := iterDir(node.(*Dir), cb); err != nil {
				return err
			}
		} else {
			if err := cb(node); err != nil {
				return err
			}
		}
	}
	return cb(dir)
}

// Borrowed from https://github.com/ipfs/go-ipfs/blob/master/fuse/mount/mount.go
func unmount(mountpoint string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("diskutil", "umount", "force", mountpoint)
	case "linux":
		cmd = exec.Command("fusermount", "-u", mountpoint)
	default:
		return fmt.Errorf("unmount: unimplemented")
	}

	errc := make(chan error, 1)
	go func() {
		defer close(errc)

		// try vanilla unmount first.
		if err := exec.Command("umount", mountpoint).Run(); err == nil {
			return
		}

		// retry to unmount with the fallback cmd
		errc <- cmd.Run()
	}()

	select {
	case <-time.After(5 * time.Second):
		return fmt.Errorf("umount timeout")
	case err := <-errc:
		return err
	}
}

type AppYAML struct {
	Name       string                 `yaml:"name"`
	EntryPoint *app.EntryPoint        `yaml:"entrypoint"`
	Config     map[string]interface{} `yaml:"config"`
}

type AppNode struct {
	fs   *FS
	meta *meta.Meta
}

func (an *AppNode) Reader() app.ReadSeekCloser {
	ff := filereader.NewFile(bfs.bs, an.meta)
	fmt.Printf("FF=%+v\n", ff)
	return ff
}

func (an *AppNode) Name() string {
	return an.meta.Name
}

func (an *AppNode) IsDir() bool {
	return an.meta.IsDir()
}

func (an *AppNode) ModTime() time.Time {
	mtime, _ := an.meta.Mtime()
	return mtime
}

func main() {
	hostPtr := flag.String("host", "", "remote host, default to http://localhost:8050")
	loglevelPtr := flag.String("loglevel", "info", "logging level (debug|info|warn|crit)")
	immutablePtr := flag.Bool("immutable", false, "make the filesystem immutable")
	hostnamePtr := flag.String("hostname", "", "default to system hostname")

	flag.Usage = Usage
	flag.Parse()

	if flag.NArg() != 2 {
		Usage()
		os.Exit(2)
	}
	name := flag.Arg(0)
	mountpoint := flag.Arg(1)

	var err error
	root.Hostname = *hostnamePtr
	if root.Hostname == "" {
		root.Hostname, err = os.Hostname()
		if err != nil {
			fmt.Printf("failed to retrieve hostname, set one manually: %v", err)
		}
	}

	lvl, err := log15.LvlFromString(*loglevelPtr)
	if err != nil {
		panic(err)
	}
	Log.SetHandler(log15.LvlFilterHandler(lvl, log15.StreamHandler(os.Stdout, log15.TerminalFormat())))
	fslog := Log.New("name", name)

	// Display stats ever 10 seconds if there was some changes in the FS
	stats = &Stats{LastReset: time.Now()}
	go func() {
		t := time.NewTicker(10 * time.Second)
		for _ = range t.C {
			// fslog.Info(fmt.Sprintf("latest=%+v,staging=%+v,mount=%+v", bfs.latest, bfs.staging, bfs.mount))
			if stats.updated {
				fslog.Info(stats.String())
				fslog.Debug("Flushing stats")
				stats.Reset()
			}
		}
	}()

	// FIXME(tsileo): re-enable, and do the update only if it's been 10 minutes without any activity
	// go func() {
	// 	t := time.NewTicker(10 * time.Minute)
	// 	for _ = range t.C {
	// 		fslog.Debug("trigger sync")
	// 		bfs.sync <- struct{}{}
	// 	}
	// }()

	sockPath := fmt.Sprintf("/tmp/blobfs_%s_%d.sock", name, time.Now().UnixNano())

	go func() {
		api := &API{}
		// TODO(tsileo): make the API port configurable
		fslog.Info("Starting API at localhost:8049")
		if err := api.Serve(sockPath); err != nil {
			fslog.Crit("failed to start API")
		}
	}()

	fslog.Info("Mouting fs...", "mountpoint", mountpoint, "immutable", *immutablePtr)
	bsOpts := blobstore.DefaultOpts().SetHost(*hostPtr, os.Getenv("BLOBSTASH_API_KEY"))
	bsOpts.SnappyCompression = false
	bs, err := cache.New(fslog.New("module", "blobstore"), bsOpts, fmt.Sprintf("blobfs_cache_%s", name))
	if err != nil {
		fslog.Crit("failed to init cache", "err", err)
		os.Exit(1)
	}

	kvsOpts := kvstore.DefaultOpts().SetHost(*hostPtr, os.Getenv("BLOBSTASH_API_KEY"))
	// FIXME(tsileo): re-enable Snappy compression
	kvsOpts.SnappyCompression = false
	rkv := kvstore.New(kvsOpts)

	c, err := fuse.Mount(
		mountpoint,
		fuse.FSName(name),
		fuse.Subtype("blobfs"),
		// fuse.LocalVolume(),
		fuse.VolumeName(name),
	)
	defer c.Close()
	if err != nil {
		fslog.Crit("failed to mount", "err", err)
		os.Exit(1)
	}

	if err := pathutil.InitVarDir(); err != nil {
		fslog.Crit("failed to setup var directory", "err", err)
		os.Exit(1)
	}

	// Initialize the local Vkv store that will store all the local mutations
	lkv, err := vkv.New(filepath.Join(pathutil.VarDir(), fmt.Sprintf("lkv_%s", name)))
	defer lkv.Close()
	if err != nil {
		panic(err)
	}

	// Retrieve the current user Uid/Gid for using it for hte FS
	cuser, err := user.Current()
	if err != nil {
		fslog.Crit("failed to get current user", "err", err)
		os.Exit(1)
	}
	iuid, err := strconv.Atoi(cuser.Uid)
	if err != nil {
		panic(err)
	}
	igid, err := strconv.Atoi(cuser.Gid)
	if err != nil {
		panic(err)
	}

	bfs = &FS{
		log:        fslog,
		socketPath: sockPath,
		name:       name,
		bs:         bs,
		c:          c,
		uid:        uint32(iuid),
		gid:        uint32(igid),
		lkv:        lkv,
		rkv:        rkv,
		uploader:   writer.NewUploader(bs),
		immutable:  *immutablePtr,
		host:       bsOpts.Host,
		cache:      map[fuse.NodeID]struct{}{},
		sync:       make(chan struct{}),
	}

	// Load the Root of the FS before we mount it
	if err := bfs.loadRoot(); err != nil {
		panic(err)
	}
	bfs.root = bfs.Mount().node.(*Dir)

	appConfigYAML, err := bfs.Path("/app.yaml")
	if err != nil {
		panic(err)
	}
	if appConfigYAML != nil {
		fakeFile := filereader.NewFile(bfs.bs, appConfigYAML.Meta())
		data, err := ioutil.ReadAll(fakeFile)
		if err != nil {
			panic(err)
		}
		fmt.Printf("YAML data=%s", data)
		appConf := &AppYAML{}
		if err := yaml.Unmarshal(data, &appConf); err != nil {
			panic(err)
		}
		fmt.Printf("YAML data=%+v", appConf)

		// func New(name string, entrypoint *EntryPoint, config map[string]interface{}, pathFunc func(string) (AppNode, error), authFunc func(*http.Request) bool) *App {
		pathFunc := func(path string) (app.AppNode, error) {
			node, err := bfs.Path(path)
			if err != nil {
				panic(err)

			}
			if node != nil {
				return &AppNode{
					fs:   bfs,
					meta: node.Meta(),
				}, err
			}

			return nil, nil
		}

		bfs.app = app.New(appConf.Name, appConf.EntryPoint, appConf.Config, pathFunc, nil)
		h := func(w http.ResponseWriter, r *http.Request) {
			bfs.app.Serve(context.TODO(), w, r)
		}
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/", h)
			http.ListenAndServe(":8030", mux)
		}()
	}
	// Listen for sync request
	// FIXME(tsileo): we may want this to be async when it's triggered when making a file public,
	// when the link will be given, it still won't be there remotely and cause issue if done pragmatically
	go func() {
		for {
			select {
			case <-bfs.sync:
				fslog.Info("Sync triggered")
				if err := bfs.Pull(); err != nil {
					fslog.Error("failed to push", "err", err)
				}
				if err := bfs.Push(nil); err != nil {
					fslog.Error("failed to push", "err", err)
				}
			}
		}
	}()

	// Actually mount the FS
	go func() {
		wg.Add(1)
		err = fs.Serve(c, bfs)
		if err != nil {
			fslog.Crit("failed to serve", "err", err)
			os.Exit(1)
		}

		// check if the mount process has an error to report
		<-c.Ready
		if err := c.MountError; err != nil {
			fslog.Crit("mount error", "err", err)
			os.Exit(1)
		}
		if err := c.Close(); err != nil {
			fslog.Crit("failed to close connection", "err", err)
		}
		bfs.bs.Close()
		wg.Done()
	}()

	// Be ready to cleanup if we receive a kill signal
	cs := make(chan os.Signal, 1)
	signal.Notify(cs, os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	<-cs
	fslog.Info("Unmounting...")
	if err := unmount(mountpoint); err != nil {
		fslog.Crit("failed to unmount", "err", err)
		os.Exit(1)
	}
	wg.Wait()
	os.Exit(0)
}

// initRoot intializes a new root dir
func (f *FS) initRoot() (*Dir, error) {
	newRoot := &Dir{
		fs:       f,
		Children: map[string]Node{},
		meta:     &meta.Meta{Name: ""}, // We want an empty name for the root
	}
	newRoot.log = f.log.New("ref", "undefined", "name", "_root", "type", "dir")
	if err := newRoot.Save(); err != nil {
		return nil, err
	}
	f.log.Debug("Created new root", "ref", newRoot.Meta().Hash)
	return newRoot, nil
}

type SyncStats struct {
	BlobsUploaded int
	BlobsSkipped  int
}

type Stats struct {
	LastReset    time.Time
	FilesCreated int
	DirsCreated  int
	FilesUpdated int
	DirsUpdated  int
	updated      bool
	sync.Mutex
}

func (s *Stats) Reset() {
	s.LastReset = time.Now()
	s.FilesCreated = 0
	s.DirsCreated = 0
	s.FilesUpdated = 0
	s.DirsUpdated = 0
	s.updated = false
}

func (s *Stats) String() string {
	return fmt.Sprintf("%d files created, %d dirs created, %d files updated, %d dirs updated",
		s.FilesCreated, s.DirsCreated, s.FilesUpdated, s.DirsUpdated)
}

// debugFile is a dummy file that hold a string
type debugFile struct {
	data []byte
}

func newDebugFile(data []byte) *debugFile {
	return &debugFile{
		data: data,
	}
}

func (f *debugFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 0
	a.Mode = 0444
	a.Size = uint64(len(f.data))
	return nil
}

func (f *debugFile) ReadAll(ctx context.Context) ([]byte, error) {
	return f.data, nil
}

type FS struct {
	log log15.Logger

	root *Dir

	rkv *kvstore.KvStore // remote vkv store
	lkv *vkv.DB          // local vkv store

	bs       *cache.Cache     // blobstore.BlobStore wrapper
	uploader *writer.Uploader // BlobStash FileTree client

	socketPath string // Socket used for HTTP FS communications

	name      string
	host      string
	immutable bool

	sync   chan struct{}
	lastOP time.Time

	local  *Mount
	remote *Mount

	c *fuse.Conn

	app *app.App

	uid uint32 // Current user uid
	gid uint32 // Current user gid

	cache map[fuse.NodeID]struct{}

	openFds int // Open file descriptors count
	mu      sync.Mutex
}

func (f *FS) InvalidateCache() error {
	for nodeID, _ := range f.cache {
		f.log.Debug("Invalidate node", "nodeID", nodeID)
		err := f.c.InvalidateNode(nodeID, 0, -1)
		switch err {
		case nil:
		case fuse.ErrNotCached:
			f.log.Debug("node not cached")
		default:
			f.log.Error("failed to invalidate", "nodeID", nodeID, "err", err)
		}
		delete(f.cache, nodeID)
	}
	// f.root.Children = nil
	return nil
}

// Mount determine if the current root should the local one or the remote one and returns it
func (f *FS) Mount() *Mount {
	if f.local != nil {
		if f.remote == nil || (f.remote != nil && f.local.root.Version > f.remote.root.Version) {
			return f.local
		}
		return f.remote
	}
	return f.remote
}

func (f *FS) Path(lp string) (Node, error) {
	return f.path(f.root, lp, "/")
}

func (f *FS) path(n Node, lp, p string) (Node, error) {
	if n.IsDir() {
		d := n.(*Dir)
		if d.Children == nil {
			if err := d.reload(); err != nil {
				return nil, err
			}
		}
		for _, child := range d.Children {

			childPath := filepath.Join(p, n.Meta().Name, child.Meta().Name)
			if child.IsDir() {
				rnode, err := f.path(child, lp, filepath.Join(p, n.Meta().Name))
				if err != nil {
					return nil, err
				}
				if rnode != nil && childPath == lp {
					return rnode, nil
				}

			} else {
				if childPath == lp {
					return child, nil
				}
			}
		}
	}
	return nil, nil
}

// Build the local index (a map[path]hash)
func (f *FS) localIndex() (map[string]string, error) {
	return f.buildLocalIndex(f.root, "/")
}

func (f *FS) buildLocalIndex(n Node, p string) (map[string]string, error) {
	index := map[string]string{}
	index[filepath.Join(p, n.Meta().Name)] = n.Meta().Hash
	if n.IsDir() {
		d := n.(*Dir)
		if d.Children == nil {
			if err := d.reload(); err != nil {
				return nil, err
			}
		}
		for _, child := range d.Children {
			if child.IsDir() {
				childIndex, err := f.buildLocalIndex(child, filepath.Join(p, n.Meta().Name))
				if err != nil {
					return nil, err
				}
				for cp, cref := range childIndex {
					index[cp] = cref
				}
			} else {
				index[filepath.Join(p, n.Meta().Name, child.Meta().Name)] = child.Meta().Hash
			}
		}
	}
	return index, nil
}

type DiffNode struct {
	Path, Hash string
}

type Diff struct {
	Added             []*DiffNode
	Conflicted        []*DiffNode
	DeletedCandidates []*DiffNode
}

func (f *FS) compareIndex(localIndex, remoteIndex map[string]string) (*Diff, error) {
	if _, ok := remoteIndex["/"]; ok {
		delete(remoteIndex, "/")
	}
	if _, ok := localIndex["/"]; ok {
		delete(localIndex, "/")
	}
	diff := &Diff{
		Added:             []*DiffNode{},
		Conflicted:        []*DiffNode{},
		DeletedCandidates: []*DiffNode{},
	}
	for p, ref := range remoteIndex {
		if lref, ok := localIndex[p]; ok {
			// The file is also present in the local index
			if ref != lref {
				// The ref are different, there is a conflict
				diff.Conflicted = append(diff.Conflicted, &DiffNode{p, ref})
			}
		} else {
			// The file is not present in the local index, it has been "added"
			diff.Added = append(diff.Added, &DiffNode{p, ref})
		}
	}
	for p, ref := range localIndex {
		if _, ok := remoteIndex[p]; !ok {
			diff.DeletedCandidates = append(diff.DeletedCandidates, &DiffNode{p, ref})
		}
	}
	// Make sure we handle the deepest children first so we don't delete a directory with a file not deleted yet
	sort.Sort(ByLength(diff.DeletedCandidates))

	return diff, nil
}

func (f *FS) updateLastOP() {
	f.lastOP = time.Now()
}

type Mount struct {
	immutable bool
	node      Node
	root      *root.Root
}

func (m *Mount) Empty() bool {
	return m.node == nil
}

func (m *Mount) Copy(m2 *Mount) {
	m2.immutable = m.immutable
	m2.node = m.node
	m2.root = m.root
}

// Same struct BlobStash's filetree.Node
// XXX(tsileo): check if we can use a Meta for this? a meta never output hash/ref :s
type RemoteNode struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Size     int     `json:"size"`
	Mode     uint32  `json:"mode"`
	ModTime  string  `json:"mtime"`
	Hash     string  `json:"ref"`
	Children []*Node `json:"children,omitempty"`
}

// remoteNode fetch the remote node at `path` in the given `mutationRef`
func (f *FS) remoteNode(mutationRef, path string) (*RemoteNode, error) {
	resp, err := f.bs.Client().DoReq("GET", fmt.Sprintf("/api/filetree/fs/ref/%s/%s", mutationRef, path), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 200:
		node := &RemoteNode{}
		if err := json.Unmarshal(body, &node); err != nil {
			return nil, err
		}
		return node, nil
	default:
		return nil, fmt.Errorf("failed to fetch node at path \"%s\" for ref=%v: %s", path, mutationRef, body)
	}
}

// remoteIndex fetch the remote index (map[path]hash) for the given mutation ref
func (f *FS) remoteIndex(ref string) (map[string]string, error) {
	resp, err := f.bs.Client().DoReq("GET", "/api/filetree/index/"+ref, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 200:
		out := map[string]string{}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, fmt.Errorf("failed to fetch index for ref=%v: %s", ref, body)
	}
}

// Refs returns a "snapshot" of the FS
// - a slice of refs containing all the blobfs of the Tree
func (f *FS) Refs(rootDir *Dir) ([]string, error) {
	f.log.Info("Fetching refs", "root", rootDir, "meta", rootDir.Meta())
	defer f.log.Info("Fetching refs done")

	wg.Add(1)
	defer wg.Done()

	f.mu.Lock()
	defer f.mu.Unlock()

	refs := []string{}

	// 	rootNode, err := bfs.getRoot()
	// 	if err != nil {
	// 		f.log.Error("Failed to fetch root", "err", err)
	// 		return nil, nil, err
	// 	}

	// 	rootDir := rootNode.(*Dir)
	// rootDir := root.node

	if err := iterDir(rootDir, func(node Node) error {
		f.log.Debug("[fetch dir]", "node", node.Meta())
		refs = append(refs, node.Meta().Hash)
		if !node.IsDir() {
			for _, iref := range node.Meta().Refs {
				data := iref.([]interface{})
				ref := data[1].(string)
				refs = append(refs, ref)
			}
		}
		return nil
	}); err != nil {
		f.log.Error("iterDir failed", "err", err)
		return nil, err
	}

	return refs, nil
}

type ByLength []*DiffNode

func (s ByLength) Len() int {
	return len(s)
}
func (s ByLength) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s ByLength) Less(i, j int) bool {
	return len(strings.Split(s[i].Path, "/")) > len(strings.Split(s[j].Path, "/"))
}

//
func (f *FS) Pull() error {
	// First, try to fetch the local root
	var err error
	var remoteRoot *root.Root
	var remoteNode Node

	fsName := fmt.Sprintf(rootKeyFmt, f.Name())
	// localFsName := fmt.Sprintf(localRootKeyFmt, f.Name())

	f.log.Debug("load latest remote mutation", "name", fsName)
	remoteKv, err := f.rkv.Get(fsName, -1)
	switch err {
	case nil:
		f.log.Debug("loaded remote", "kv", string(remoteKv.Data))
		// There are mutations for this FS in BlobStash
		remoteRoot, remoteNode, err = f.kvDataToDir(remoteKv.Data, remoteKv.Version)
	case kvstore.ErrKeyNotFound:
		f.log.Debug("remote not found")
		// The FS is new, no remote mutation nor local, we'll create the inital root later
	default:
		f.log.Error("failed to fetch lastest mutation from BlobStash", "err", err)
		return err
	}

	// Then, try to fetch the remote root
	f.log.Debug("load latest local mutation")
	localKv, err := f.lkv.Get(fsName, -1)
	switch err {
	case nil:
		f.log.Debug("loaded local", "kv", string(localKv.Data))
	case vkv.ErrNotFound:
		f.log.Debug("local not found")
	default:
		return err
	}

	switch {
	case localKv == nil:
		// FIXME(tsileo): is this case even possible?
		f.log.Debug("No local mutations yet")
		if remoteKv == nil {
			newRoot, err := f.initRoot()
			if err != nil {
				return err
			}
			rootNode := newRoot
			// The root was just created
			localRoot := &root.Root{Ref: rootNode.Meta().Hash}
			jsroot, err := localRoot.JSON()
			if err != nil {
				return err
			}
			localFsName := fmt.Sprintf(localRootKeyFmt, f.Name())
			kv, err := f.lkv.Put(localFsName, "", jsroot, -1)
			localRoot.Version = kv.Version
			if err != nil {
				return err
			}
			f.local = &Mount{
				immutable: false,
				root:      localRoot,
				node:      newRoot,
			}
			f.root = f.Mount().node.(*Dir)
			return nil
		}
		// Fetch and save all the known remote mutations
		versions, err := f.rkv.Versions(fsName, 0, -1, 0)
		if err != nil {
			return err
		}
		for _, version := range versions.Versions {
			f.log.Debug("Saving mutation locally", "root", string(version.Data))
			if f.lkv.Put(fsName, version.Hash, version.Data, version.Version); err != nil {
				return err
			}
		}
		f.remote = &Mount{
			immutable: f.Immutable(),
			root:      remoteRoot,
			node:      remoteNode,
		}
		f.log.Debug("DEBUG", "f.root", f.root, "remoteDir", remoteNode)
		if f.root != nil {
			*f.root = *remoteNode.(*Dir)
		} else {
			f.root = remoteNode.(*Dir)
		}
		// if err := bfs.c.InvalidateEntry(fuse.RootID, ""); err != nil {
		// 	f.log.Error("failed to invalidate entry", "err", err)
		// 	return err
		// }
		return f.InvalidateCache()

	case remoteKv == nil:
		f.log.Info("FS does not exist remotely")

	case remoteKv.Version > localKv.Version:
		f.log.Info("there are un-synced remote mutations")
		// No un-synced mutation, just copy the new mutations
		// versions, err := f.rkv.Versions(fsName, localKv.Version-1, -1, 0)
		versions, err := f.rkv.Versions(fsName, 0, -1, 0)
		if err != nil {
			return err
		}

		// FIXME(tsileo): assert that the latest remote (the one stored locally) ref is
		// actually present in the old versions
		// var lastRefData []byte
		saved := 0
		shouldBreak := false
		for _, version := range versions.Versions {
			if shouldBreak {
				break
			}
			if f.remote != nil && version.Version == f.remote.root.Version {
				shouldBreak = true
				// This mean we should catch the version as the previous ref
				continue
			}
			if f.lkv.Put(fsName, version.Hash, version.Data, version.Version); err != nil {
				return err
			}
			saved++
		}

		f.log.Info("Remote mutations saved", "count", saved)

		// FIXME(tsileo): check here too
		// Check we have mutation not synced yet
		if f.local != nil && f.local.root.Version > localKv.Version {
			// Conflict handling

			// FIXME(tsileo): do a merge, create a new mount and set it as local
			f.log.Info("There is a conflict")

			remoteIndex, err := f.remoteIndex(remoteRoot.Ref)
			if err != nil {
				return err
			}
			f.log.Info("Fetched remote index", "index", remoteIndex)

			localIndex, err := f.localIndex()
			if err != nil {
				return err
			}
			f.log.Info("Built local index", "index", localIndex)

			// Compute the diff between the two mutations
			diff, err := f.compareIndex(localIndex, remoteIndex)
			if err != nil {
				return err
			}
			f.log.Info("Computed diff", "diff", diff)

			for _, added := range diff.Added {
				m, err := f.metaFromHash(added.Hash)
				if err != nil {
					return err
				}
				f.log.Info("[add]", "node", added)
				if err := f.createNode(added.Path, m); err != nil {
					return err
				}
			}

			for _, conflicted := range diff.Conflicted {
				m, err := f.metaFromHash(conflicted.Hash)
				if err != nil {
					return err
				}
				f.log.Info("[conflicted]", "node", conflicted)
				m.Name = m.Name + ".conflicted"
				if err := f.createNode(conflicted.Path+".conflicted", m); err != nil {
					return err
				}
			}
			// If there is only one remote mutation, then all the deletedCandidates are new local files
			// if prevMutationRef != "" {
			// FIXME(tsileo): rename Diff.Deleted to Diff.DeletedCandidates and make the handling outside of this func
			// then, check at /api/filetree/fs/ref/{ref}+p
			// if the node exists, compare the ref, if it's the same, we can delete the file
			// safely (since it will be super easy to restore), it it's not the same,
			// rename it as .conflicted+deleted
			// }
			// FIXME(tsileo): check if there is a previous version

			for _, deletedCandidate := range diff.DeletedCandidates {
				// rnode, err := f.remoteNode()
				f.log.Debug("[deleted *candidate*]", "node", deletedCandidate)
				// 	f.log.Info("[deleted]", "node", deleted)
				// 	// FIXME(tsileo): detect new file/unsynced file/if the deleted file has been modified"
				// 	// XXX(tsileo): should check the latest remote (from local rkv) and see if the file is the same
				// 	// in this case delete it, if not ???
				// 	if err := f.deleteNode(deleted.Path); err != nil {
				// 		return err
				// 	}
			}

			// FIXME(tsileo): bad root here?
			// f.remote = &Mount{
			// 	immutable: f.Immutable(),
			// 	node:
			// }

			*f.root = *f.local.node.(*Dir)
			f.log.Info("Diff done")

			return f.InvalidateCache()
		}

		f.remote = &Mount{
			immutable: f.Immutable(),
			root:      remoteRoot,
			node:      remoteNode,
		}
		*f.root = *remoteNode.(*Dir)

	case remoteKv.Version < localKv.Version:
		return fmt.Errorf("BlobStash seems out of sync")
	case localKv.Version == remoteKv.Version:
		f.log.Info("Already in sync")
		return nil
	}

	return f.InvalidateCache()
}

func (f *FS) metaFromHash(hash string) (*meta.Meta, error) {
	blob, err := f.bs.Get(context.TODO(), hash)
	if err != nil {
		return nil, err
	}
	// Decode it as a Meta
	return meta.NewMetaFromBlob(hash, blob)
}

func (f *FS) deleteNode(path string) error {
	split := strings.Split(path[1:], "/")
	pathCount := len(split)
	node := f.root
	for i, p := range split {
		if node.Children == nil {
			if err := node.reload(); err != nil {
				return err
			}
		}
		child, ok := node.Children[p]
		if ok {
			if i == pathCount-1 {
				delete(node.Children, p)
				return node.Save()
			}

			// Keep searching
			node = child.(*Dir)
			continue
		}

		return fmt.Errorf("shouldn't happen")
	}
	return nil
}

func (f *FS) createNode(path string, cmeta *meta.Meta) error {
	var prev *Dir
	split := strings.Split(path[1:], "/")
	pathCount := len(split)
	node := f.root
	for i, p := range split {
		if node.Children == nil {
			if err := node.reload(); err != nil {
				return err
			}
		}
		prev = node
		child, ok := node.Children[p]
		if ok {
			node = child.(*Dir)
			continue
		}

		if i == pathCount-1 {
			nfile, err := NewFile(f, cmeta, node)
			if err != nil {
				return err
			}
			node.Children[p] = nfile
			if err := node.Save(); err != nil {
				return err
			}

		} else {
			newMeta := &meta.Meta{
				Type: "dir",
				Name: p,
			}
			newd, err := NewDir(f, newMeta, prev)
			if err != nil {
				return err
			}
			node.Children[p] = newd
			// FIXME(tsileo): needed?
			if err := node.Save(); err != nil {
				return err
			}
			node = newd
		}
	}
	return nil
}

// Push saves all the blobs of the tree, and add the VK entry to the remote BlobStash instance
func (f *FS) Push(comment []byte) error {
	f.log.Info("Pushing data", "comment", comment)

	wg.Add(1)
	defer wg.Done()

	// Ensure the current root is a local one
	if f.Mount().root.Ref != f.local.root.Ref {
		f.log.Info("No local changes")
		return nil
	}

	// Try to fetch the latest remote mutation
	fsName := fmt.Sprintf(rootKeyFmt, f.Name())
	remoteKv, err := f.rkv.Get(fsName, -1)
	// versions, err2 := f.rkv.Versions(fsName, 0, -1, 0)
	// if err2 != nil && err2 != kvstore.ErrKeyNotFound {
	// 	panic(err)
	// }
	// if versions != nil {
	// 	fmt.Printf("DEBUG:%+v/\n%+v/\n%+v/\n%+v\n\n", remoteKv, versions.Versions[0], f.remote.root, f.remote.node)
	// }
	switch err {
	case nil:
		// There are mutations for this FS in BlobStash
		_, remoteNode, err := f.kvDataToDir(remoteKv.Data, remoteKv.Version)
		f.log.Debug("remote node", "node", remoteNode)
		if err != nil {
			return err
		}
		// FIXME(tsileo): compare with lkv instead f.remote
		// if f.remote.root != nil && f.remote.root.Ref != remoteRoot.Ref {
		// 	f.log.Error("conflicted", "local_remote_root", f.remote.root, "remote_root", remoteRoot)
		// 	// FIXME(tsileo): return conflicted error asking to pull
		// 	return nil
		// }
	case kvstore.ErrKeyNotFound:
		// The FS is new, no remote mutation nor local, we'll create the inital root later
	default:
		f.log.Error("failed to fetch lastest mutation from BlobStash", "err", err)
		return err
	}

	// Keep some basic stats about the on-going sync
	stats := &SyncStats{}
	defer f.log.Info("Push done", "blobs_uploaded", stats.BlobsUploaded, "blobs_skipped", stats.BlobsSkipped)

	// rootNode := f.root

	// rootDir := f.local.node.(*Dir) //rootNode.(*Dir)
	croot := f.local.root
	if comment != nil {
		croot.Comment = string(comment)
	}

	refs, err := bfs.Refs(f.root)
	if err != nil {
		return err
	}
	f.log.Debug("snapshot fetched", "root", croot, "len", len(refs))

	// First save all the blobs of the tree
	for _, ref := range refs {
		exists, err := f.bs.StatRemote(ref)
		if err != nil {
			f.log.Error("stat failed", "err", err)
			return err
		}
		if exists {
			stats.BlobsSkipped++
		} else {
			blob, err := f.bs.Get(context.TODO(), ref)
			if err != nil {
				f.log.Error("Failed to fetch blob from cached", "err", err)
			}
			if err := f.bs.PutRemote(ref, blob); err != nil {
				f.log.Error("PutRemote failed", "err", err)
				return err
			}
			stats.BlobsUploaded++
		}
	}

	jsRoot, err := croot.JSON()
	if err != nil {
		return err
	}
	// Set a KV entry for this mutation
	// FIXME(tsileo): conditional request to ensure the previous version is the same
	f.log.Debug("saving the mutation remotely", "name", fsName, "version", croot.Version, "ref", croot.Ref)
	if _, err := bfs.rkv.Put(fsName, "", jsRoot, croot.Version); err != nil {
		f.log.Error("Sync failed (failed to update the remote vkv entry)", "err", err)
		return err
	}
	// Save the mutation as remote locally  too
	if _, err := bfs.lkv.Put(fsName, "", jsRoot, croot.Version); err != nil {
		f.log.Error("Sync failed (failed to update the remote vkv entry)", "err", err)
		return err
	}

	return nil
}

func (f *FS) Immutable() bool {
	// TODO(tsileo): check the mount
	return f.immutable
}

func (f *FS) Name() string {
	return f.name
}

var (
	rootKeyFmt      = "blobfs:root:%v"
	localRootKeyFmt = "local:root:%v"
)

func (f *FS) Root() (fs.Node, error) {
	f.log.Info("OP Root")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Mount().node, nil
}

func (f *FS) kvDataToDir(data []byte, version int) (*root.Root, *Dir, error) {
	lroot, err := root.NewFromJSON([]byte(data), version)
	if err != nil {
		return nil, nil, err
	}
	f.log.Debug("decoding root", "root", lroot)
	// Fetch the root ref
	blob, err := f.bs.Get(context.TODO(), lroot.Ref)
	if err != nil {
		return nil, nil, err
	}
	// Decode it as a Meta
	m, err := meta.NewMetaFromBlob(lroot.Ref, blob)
	if err != nil {
		return nil, nil, err
	}
	f.log.Debug("loaded meta root", "ref", m.Hash)
	dir, err := NewDir(f, m, nil)
	if err != nil {
		return nil, nil, err
	}
	return lroot, dir, nil
}

func (f *FS) loadRoot() error {
	// First, try to fetch the local root
	// return f.Pull()
	var err error
	var wipRoot, localRoot, remoteRoot *root.Root
	var wipNode, localNode, remoteNode, rootNode Node

	fsName := fmt.Sprintf(rootKeyFmt, f.Name())
	localFsName := fmt.Sprintf(localRootKeyFmt, f.Name())

	f.log.Debug("load latest local mutation")
	localKv, err := f.lkv.Get(fsName, -1)
	switch err {
	case nil:
		localRoot, localNode, err = f.kvDataToDir(localKv.Data, localKv.Version)
	case vkv.ErrNotFound:
	default:
		return err
	}

	f.log.Debug("load latest wip mutation")
	wipKv, err := f.lkv.Get(localFsName, -1)
	switch err {
	case nil:
		wipRoot, wipNode, err = f.kvDataToDir(wipKv.Data, wipKv.Version)
	case vkv.ErrNotFound:
	default:
		return err
	}

	// Then, try to fetch the remote root
	f.log.Debug("load latest remote mutation")
	remoteKv, err := f.rkv.Get(fsName, -1)
	switch err {
	case nil:
		// There are mutations for this FS in BlobStash
		remoteRoot, remoteNode, err = f.kvDataToDir(remoteKv.Data, remoteKv.Version)
		f.log.Debug("remote node", "node", remoteNode)
	case kvstore.ErrKeyNotFound:
		// The FS is new, no remote mutation nor local, we'll create the inital root later
	default:
		f.log.Error("failed to fetch lastest mutation from BlobStash", "err", err)
		return err
	}
	if wipKv != nil && ((localKv == nil && remoteKv == nil) || (remoteKv != nil && localKv == nil && wipKv.Version > remoteKv.Version) || (localKv != nil && remoteKv != nil && wipKv.Version > localKv.Version && wipKv.Version > remoteKv.Version)) {
		f.local = &Mount{
			immutable: f.Immutable(),
			node:      wipNode,
			root:      wipRoot,
		}
		f.root = f.Mount().node.(*Dir)
		return nil
	}
	switch {
	case localKv == nil && remoteKv == nil:
		newRoot, err := f.initRoot()
		if err != nil {
			return err
		}
		rootNode = newRoot
		// The root was just created
		localRoot = &root.Root{Ref: rootNode.Meta().Hash}
		jsroot, err := localRoot.JSON()
		if err != nil {
			return err
		}
		kv, err := f.lkv.Put(localFsName, "", jsroot, -1)
		localRoot.Version = kv.Version
		if err != nil {
			return err
		}
		f.local = &Mount{
			immutable: false,
			root:      localRoot,
			node:      newRoot,
		}
		f.root = f.Mount().node.(*Dir)
		return nil
	case localKv != nil && remoteKv != nil:
		if localRoot.Version == remoteRoot.Version {
			f.remote = &Mount{
				immutable: f.Immutable(),
				node:      localNode,
				root:      localRoot,
			}
			return nil
		}
		if localKv.Version > remoteKv.Version {
			f.log.Error("Version mismatch", "localkv", localKv, "remotekv", remoteKv)
			// XXX(tsileo): recover from this should be possible if the cache hasn't been pruned
			return fmt.Errorf("BlobStash instance seems out of sync, version mismatch")
		} else {
			// FIXME(tsileo): not only save the last, but all the missing one

			localKv, err = f.lkv.Put(fsName, remoteKv.Hash, remoteKv.Data, remoteKv.Version)
			if err != nil {
				return err
			}
			f.remote = &Mount{
				immutable: f.Immutable(),
				node:      remoteNode,
				root:      remoteRoot,
			}
			f.root = f.Mount().node.(*Dir)
			return nil
		}
	case remoteKv != nil && localKv == nil:
		f.log.Debug("Saving the remote mutations locally")
		versions, err := f.rkv.Versions(fsName, 0, -1, 0)
		if err != nil {
			return err
		}
		for _, version := range versions.Versions {
			if f.lkv.Put(fsName, version.Hash, version.Data, version.Version); err != nil {
				return err
			}
		}

		f.remote = &Mount{
			immutable: f.Immutable(),
			node:      remoteNode,
			root:      remoteRoot,
		}
		f.root = f.Mount().node.(*Dir)
		return nil
	}
	return fmt.Errorf("shouldn't happen")
}

// the Node interface wraps `fs.Node`
type Node interface {
	fs.Node
	Meta() *meta.Meta
	SetMeta(*meta.Meta)
	Save() error
	IsDir() bool
}

// Dir implements both Node and Handle for the root directory.
type Dir struct {
	fs       *FS
	meta     *meta.Meta
	parent   *Dir
	Children map[string]Node
	log      log15.Logger
}

func NewDir(rfs *FS, m *meta.Meta, parent *Dir) (*Dir, error) {
	d := &Dir{
		fs:     rfs,
		meta:   m,
		parent: parent,
		log:    rfs.log.New("ref", m.Hash, "name", m.Name, "type", "dir"),
	}
	return d, nil
}

func (d *Dir) reload() error {
	// XXX(tsileo): should we assume the Mutex is locked?
	d.log.Info("Reload dir children")
	d.Children = map[string]Node{}
	for _, ref := range d.meta.Refs {
		d.log.Debug("Trying to fetch ref", "hash", ref.(string))
		blob, err := d.fs.bs.Get(context.TODO(), ref.(string))
		if err != nil {
			return err
		}
		m, err := meta.NewMetaFromBlob(ref.(string), blob)
		if err != nil {
			return err
		}
		d.log.Debug("fetched meta", "meta", m)
		if m.IsDir() {
			ndir, err := NewDir(d.fs, m, d)
			if err != nil {
				d.log.Error("failed to build dir", "err", err)
				return err
			}
			d.Children[m.Name] = ndir
		} else {
			nfile, err := NewFile(d.fs, m, d)
			if err != nil {
				d.log.Error("failed to build file", "err", err)
				return err
			}
			d.Children[m.Name] = nfile
		}
	}
	return nil
}

func (d *Dir) IsDir() bool { return true }

func (d *Dir) Meta() *meta.Meta { return d.meta }

func (d *Dir) SetMeta(m *meta.Meta) {
	d.meta = m
}

func (d *Dir) Attr(ctx context.Context, a *fuse.Attr) error {
	d.log.Debug("OP Attr")
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	if d.parent == nil {
		// Root should have Inode 2
		a.Inode = 2
	} else {
		a.Inode = 0
	}

	a.Mode = os.ModeDir | 0555
	a.Uid = d.fs.uid
	a.Gid = d.fs.gid
	return nil
}

func makePublic(node Node, value string) error {
	if value == "1" {
		node.Meta().XAttrs["public"] = value
	} else {
		delete(node.Meta().XAttrs, "public")
	}
	// TODO(tsileo): too much mutations??
	if node.IsDir() {
		for _, child := range node.(*Dir).Children {
			if err := makePublic(child, value); err != nil {
				return err
			}
		}
	}
	if err := node.Save(); err != nil {
		return err
	}
	return nil
}

func (d *Dir) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	d.log.Debug("OP Setxattr", "name", req.Name, "xattr", string(req.Xattr))
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	// If the request is to make the dir public, make it recursively
	if req.Name == "public" {
		return makePublic(d, string(req.Xattr))
	}

	// Prevent writing attributes name that are virtual attributes
	if _, exists := virtualXAttrs[req.Name]; exists {
		return nil
	}

	if d.meta.XAttrs == nil {
		d.meta.XAttrs = map[string]string{}
	}

	d.meta.XAttrs[req.Name] = string(req.Xattr)

	if err := d.Save(); err != nil {
		return err
	}

	// // Trigger a sync so the file will be (un)available for BlobStash right now
	if req.Name == "public" {
		bfs.sync <- struct{}{}
	}

	return nil
}

func (d *Dir) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	d.log.Debug("OP Removexattr", "name", req.Name)
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	// Can't delete virtual attributes
	if _, exists := virtualXAttrs[req.Name]; exists {
		return fuse.ErrNoXattr
	}

	if d.meta.XAttrs == nil {
		return fuse.ErrNoXattr
	}

	if _, ok := d.meta.XAttrs[req.Name]; ok {
		// Delete the attribute
		delete(d.meta.XAttrs, req.Name)
		if err := d.Save(); err != nil {
			return err
		}

		// // Trigger a sync so the file won't be available via BlobStash
		if req.Name == "public" {
			bfs.sync <- struct{}{}
		}

		return nil
	}
	return fuse.ErrNoXattr
}

func (d *Dir) Forget() {
	d.log.Debug("OP Forget")
	// For now, this is a noop
}

func (d *Dir) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	d.log.Debug("OP Listxattr")
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	return handleListxattr(d.meta, resp)
}

func (d *Dir) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	d.log.Debug("OP Getxattr", "name", req.Name)
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	return handleGetxattr(d.fs, d.meta, req, resp)
}

func (d *Dir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	d.log.Debug("OP Rename", "name", req.OldName, "new_name", req.NewName)
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	if d.Children == nil {
		if err := d.reload(); err != nil {
			return err
		}
	}

	if node, ok := d.Children[req.OldName]; ok {
		meta := node.Meta()
		// FIXME(tsileo): Is the RenameMeta even needed? (it may be bad that it's saving the meta?)
		if err := d.fs.uploader.RenameMeta(meta, req.NewName); err != nil {
			return err
		}
		// Delete the source
		delete(d.Children, req.OldName)

		ndir := newDir.(*Dir)
		if d != ndir {
			ndir.Children[req.NewName] = node
		} else {
			d.Children[req.NewName] = node
		}

		if err := d.Save(); err != nil {
			return err
		}

		// Also save the dest dir if it was different from the src dir
		if d != ndir {
			if err := ndir.Save(); err != nil {
				return err
			}
		}
		return nil
	}

	return fuse.EIO
}

func (d *Dir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (fs.Node, error) {
	name := req.Name
	d.log.Debug("OP Lookup", "name", name, "req", req, "resp", resp)
	resp.EntryValid = 5 * time.Second
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	// Magic file for returnign the socket path, available in every directory
	if name == ".blobfs_socket" {
		return newDebugFile([]byte(d.fs.socketPath)), nil
	}

	// normal lookup operation
	if d.Children == nil {
		if err := d.reload(); err != nil {
			return nil, err
		}
	}

	var debug bool
	if strings.HasSuffix(name, debugSuffix) {
		debug = true
		name = name[0 : len(name)-len(debugSuffix)]
	}

	if c, ok := d.Children[name]; ok {
		// If we are in debug, output the Meta as JSON (hash + meta JSON encoded)
		if debug {
			hash, js := c.Meta().Json()
			payload := []byte(hash)
			payload = append(payload, js...)
			return newDebugFile(payload), nil
		}
		return c, nil
	}

	return nil, fuse.ENOENT
}

func (d *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	d.log.Debug("OP ReadDirAll")
	d.fs.updateLastOP()

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	if d.Children == nil {
		if err := d.reload(); err != nil {
			return nil, err
		}
	}

	dirs := []fuse.Dirent{}
	for _, c := range d.Children {
		nodeType := fuse.DT_File
		if c.IsDir() {
			nodeType = fuse.DT_Dir
		}

		dirs = append(dirs, fuse.Dirent{
			Inode: 0,
			Name:  c.Meta().Name,
			Type:  nodeType,
		})
	}
	return dirs, nil
}

func (d *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	d.log.Debug("OP Mkdir", "name", req.Name)
	d.fs.updateLastOP()

	if d.fs.Immutable() {
		return nil, fuse.EPERM
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	if d.Children == nil {
		if err := d.reload(); err != nil {
			return nil, err
		}
	}

	// Ensure the directory does not already exist
	if _, ok := d.Children[req.Name]; ok {
		return nil, fuse.EEXIST
	}

	// XXX(tsileo): can permissions be set when creating a dir? if so handle it

	// Actually create the dir
	newdir := &Dir{
		fs:       d.fs,
		parent:   d,
		Children: map[string]Node{},
		// Put only the name in the Meta since when saving it will set oll the needed attrs
		meta: &meta.Meta{
			Name: req.Name,
		},
	}
	newdir.log = d.fs.log.New("ref", "unknown", "name", req.Name, "type", "dir")

	// Save it
	if err := newdir.Save(); err != nil {
		return nil, err
	}

	// Make this new the dir the children of its parent
	d.Children[newdir.meta.Name] = newdir
	if err := d.Save(); err != nil {
		return nil, err
	}
	newdir.log = newdir.log.New("ref", newdir.meta.Hash)

	stats.Lock()
	stats.updated = true
	stats.DirsCreated++
	stats.Unlock()

	return newdir, nil
}

func (d *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	d.log.Debug("OP Remove", "name", req.Name)
	d.fs.updateLastOP()

	if d.fs.Immutable() {
		return fuse.EPERM
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	if d.Children == nil {
		if err := d.reload(); err != nil {
			return err
		}
	}

	// FIXME(tsileo): what happens when trying to remove a file that does not exist?
	delete(d.Children, req.Name)
	if err := d.Save(); err != nil {
		d.log.Error("Failed to saved", "err", err)
		return err
	}

	return nil
}

// Save save all the node recursively bottom to top until the root node is reached
// Assumes the caller has acquired the lock
func (d *Dir) Save() error {
	d.log.Debug("saving")

	// Create a new Meta and populate it using the previous Meta data
	m := meta.NewMeta()
	m.Name = d.meta.Name
	m.Type = "dir"
	m.Mode = uint32(os.ModeDir | 0555)
	if d.meta.ModTime != "" {
		m.ModTime = d.meta.ModTime
	} else {
		m.ModTime = time.Now().Format(time.RFC3339)
	}

	for _, c := range d.Children {
		switch node := c.(type) {
		case *Dir:
			m.AddRef(node.meta.Hash)
		case *File:
			m.AddRef(node.meta.Hash)
		}
	}

	// Recompute the hash and update the node's meta ref
	mhash, mjs := m.Json()
	m.Hash = mhash
	d.meta = m

	mexists, err := d.fs.bs.Stat(mhash)
	if err != nil {
		d.log.Error("stat failed", "err", err)
		return err
	}

	if !mexists {
		if err := d.fs.bs.Put(mhash, mjs); err != nil {
			d.log.Error("put failed", "err", err)
			return err
		}
	}

	if d.parent == nil {
		// If no parent, this is the root so save the ref
		root := root.New(mhash, 0)
		js, err := json.Marshal(root)
		if err != nil {
			return err
		}

		// Save the mutation locally
		kv, err := d.fs.lkv.Put(fmt.Sprintf(localRootKeyFmt, d.fs.Name()), "", js, -1)
		if err != nil {
			return err
		}

		root.Version = kv.Version
		d.log.Debug("Creating a new VKV entry", "entry", kv)

		// Update the local mount
		d.fs.local = &Mount{
			immutable: false,
			root:      root,
			node:      d,
		}

		d.log.Debug("Current root", "root", d.fs.root, "new", d)

		// FIXME(tsileo): should the root be updated??
		if d.fs.root != nil {
			*d.fs.root = *d
		}
	} else {
		// d.parent.mu.Lock()
		// defer d.parent.mu.Unlock()
		if err := d.parent.Save(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	d.log.Debug("OP Create", "name", req.Name)
	d.fs.updateLastOP()

	if d.fs.Immutable() {
		return nil, nil, fuse.EPERM
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	if d.Children == nil {
		if err := d.reload(); err != nil {
			return nil, nil, err
		}
	}

	m := meta.NewMeta()
	m.Type = "file"
	m.Name = req.Name
	m.Mode = uint32(req.Mode)
	m.ModTime = time.Now().Format(time.RFC3339)

	// If the parent directory is public, the new file should to
	if d.meta.IsPublic() {
		m.XAttrs = map[string]string{"public": "1"}
	}

	// Save the meta
	mhash, mjs := m.Json()
	m.Hash = mhash
	mexists, err := d.fs.bs.Stat(mhash)
	if err != nil {
		return nil, nil, err
	}
	if !mexists {
		if err := d.fs.bs.Put(mhash, mjs); err != nil {
			return nil, nil, err
		}
	}

	// Create the file node and set it as the children of the parent
	f, err := NewFile(d.fs, m, d)
	if err != nil {
		return nil, nil, err
	}
	d.Children[m.Name] = f
	if err := d.Save(); err != nil {
		return nil, nil, err
	}

	// XXX(tsileo): track opened file descriptor globally in the FS?
	f.state.openCount++
	f.fs.openFds++
	f.log.Debug("new openCount", "count", f.state.openCount, "global", f.fs.openFds)

	stats.Lock()
	stats.updated = true
	stats.FilesCreated++
	stats.Unlock()

	return f, f, nil
}

type fileState struct {
	updated   bool
	openCount int
}

type File struct {
	fs       *FS
	data     []byte // FIXME(tsileo): if data grows too much, use a temp file
	meta     *meta.Meta
	FakeFile *filereader.File
	log      log15.Logger
	parent   *Dir
	state    *fileState
}

func NewFile(fs *FS, m *meta.Meta, parent *Dir) (*File, error) {
	return &File{
		parent: parent,
		fs:     fs,
		meta:   m,
		log:    fs.log.New("ref", m.Hash, "name", m.Name, "type", "file"),
		state:  &fileState{},
	}, nil
}

func (f *File) IsDir() bool { return false }

func (f *File) Meta() *meta.Meta { return f.meta }

func (f *File) SetMeta(m *meta.Meta) {
	f.meta = m
}

func (f *File) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	f.log.Debug("OP Write", "offset", req.Offset, "size", len(req.Data))
	f.fs.updateLastOP()

	if f.fs.Immutable() {
		return fuse.EPERM
	}

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	// Set the updated flag
	f.state.updated = true

	newLen := req.Offset + int64(len(req.Data))
	if newLen > int64(maxInt) {
		return fuse.Errno(syscall.EFBIG)
	}

	n := copy(f.data[req.Offset:], req.Data)
	if n < len(req.Data) {
		f.data = append(f.data, req.Data[n:]...)
	}

	resp.Size = len(req.Data)
	return nil
}

// XXX(tsileo): try to get rid of this
type ClosingBuffer struct {
	*bytes.Buffer
}

func (*ClosingBuffer) Close() error {
	return nil
}

func (f *File) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	f.log.Debug("OP Flush")
	f.fs.updateLastOP()

	// Flush is a noop for now

	return nil
}

func (f *File) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	f.log.Debug("OP Setxattr", "name", req.Name, "xattr", string(req.Xattr))
	f.fs.updateLastOP()

	if f.fs.Immutable() {
		return nil
	}

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	// Prevent writing attributes name that are virtual attributes
	if _, exists := virtualXAttrs[req.Name]; exists {
		return nil
	}

	if f.meta.XAttrs == nil {
		f.meta.XAttrs = map[string]string{}
	}
	f.meta.XAttrs[req.Name] = string(req.Xattr)

	// XXX(tsileo): check thath the parent get the updated hash?
	if err := f.Save(); err != nil {
		return err
	}

	// Trigger a sync so the file will be (un)available for BlobStash right now
	if req.Name == "public" {
		bfs.sync <- struct{}{}
	}
	return nil
}

// Save will save every node recursively bottom to top until the root is reached.
// Assumes the FS lock is acquired.
func (f *File) Save() error {
	if f.fs.Immutable() {
		f.log.Warn("Trying to save an immutable node")
		return nil
	}

	// Update the new `Meta`
	f.log.Debug("OP Save (file)", "meta", f.meta)
	// f.parent.fs.uploader.PutMeta(f.meta)

	// And save the parent
	return f.parent.Save()
}

func handleListxattr(m *meta.Meta, resp *fuse.ListxattrResponse) error {
	// Add the "virtual" eXtended Attributes
	for vattr, xattrFunc := range virtualXAttrs {
		if xattrFunc != nil {
			resp.Append(vattr)
		}
	}

	if m.XAttrs == nil {
		return nil
	}

	for k, _ := range m.XAttrs {
		resp.Append(k)
	}

	if m.IsPublic() {
		resp.Append("url")
	}

	return nil
}

func (f *File) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	f.log.Debug("OP Listxattr")
	f.fs.updateLastOP()

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	return handleListxattr(f.meta, resp)
}

func (f *File) Forget() {
	f.log.Debug("OP Forget")
	// Forget is a noop for now
}

func handleGetxattr(fs *FS, m *meta.Meta, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	fs.log.Debug("handleGetxattr", "name", req.Name)

	// Check if the request match a virtual extended attributes
	if xattrFunc, ok := virtualXAttrs[req.Name]; ok && xattrFunc != nil {
		resp.Xattr = xattrFunc(m)
		return nil
	}

	if req.Name == "url.semiprivate" {
		client := fs.bs.Client()
		nodeResp, err := client.DoReq("HEAD", "/api/filetree/node/"+m.Hash+"?bewit=1", nil, nil)
		if err != nil {
			return err
		}
		if nodeResp.StatusCode != 200 {
			return fmt.Errorf("bad status code: %d", nodeResp.StatusCode)
		}
		bewit := nodeResp.Header.Get("BlobStash-FileTree-Bewit")
		raw_url := fmt.Sprintf("%s/%s/%s?bewit=%s", fs.host, m.Type[0:1], m.Hash, bewit)
		resp.Xattr = []byte(raw_url)
		return nil
	}

	if req.Name == "url" && m.IsPublic() {
		// Ensure the node is public
		// FIXME(tsileo): fetch the hostname from `bfs` to reconstruct an absolute URL
		// Output the URL
		raw_url := fmt.Sprintf("%s/%s/%s", fs.host, m.Type[0:1], m.Hash)
		resp.Xattr = []byte(raw_url)
		return nil
	}

	if m.XAttrs == nil {
		return fuse.ErrNoXattr
	}

	if _, ok := m.XAttrs[req.Name]; ok {
		resp.Xattr = []byte(m.XAttrs[req.Name])
		return nil
	}

	return fuse.ErrNoXattr
}

func (f *File) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	f.log.Debug("OP Getxattr", "name", req.Name)
	f.fs.updateLastOP()

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	return handleGetxattr(f.parent.fs, f.meta, req, resp)
}

func (f *File) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	f.log.Debug("OP Removexattr", "name", req.Name)
	f.fs.updateLastOP()

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	// Can't delete virtual attributes
	if _, exists := virtualXAttrs[req.Name]; exists {
		return fuse.ErrNoXattr
	}

	if f.meta.XAttrs == nil {
		return fuse.ErrNoXattr
	}

	if _, ok := f.meta.XAttrs[req.Name]; ok {
		// Delete the attribute
		delete(f.meta.XAttrs, req.Name)

		// Save the meta
		if err := f.Save(); err != nil {
			return err
		}
		// Trigger a sync so the file won't be available via BlobStash
		if req.Name == "public" {
			bfs.sync <- struct{}{}
		}

		return nil
	}

	return fuse.ErrNoXattr
}

func (f *File) Size() int {
	if f.fs.Immutable() || f.data == nil {
		return f.meta.Size
	} else {
		// If the file is open, check the buffer length
		return len(f.data)
	}
}

func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	f.log.Debug("OP Attr")
	f.fs.updateLastOP()

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	a.Inode = 0 // auto inode
	a.Mode = os.FileMode(f.meta.Mode)
	a.Uid = f.fs.uid
	a.Gid = f.fs.gid
	a.Size = uint64(f.Size())

	if f.meta.ModTime != "" {
		t, err := time.Parse(time.RFC3339, f.meta.ModTime)
		if err != nil {
			panic(fmt.Errorf("error parsing mtime for %v: %v", f, err))
		}
		a.Mtime = t
	}

	f.log.Debug("attrs", "a", a)

	return nil
}

func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	f.log.Debug("OP Setattr")
	f.fs.updateLastOP()

	if f.fs.Immutable() {
		return fuse.EPERM
	}

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	// FIXME(tsileo): implement this
	//if req.Valid&fuse.SetattrMode != 0 {
	//if err := os.Chmod(n.path, req.Mode); err != nil {
	//	return err
	//}
	//	log.Printf("Setattr %v chmod", f.Meta.Name)
	//}
	//if req.Valid&(fuse.SetattrUid|fuse.SetattrGid) != 0 {
	//	if req.Valid&fuse.SetattrUid&fuse.SetattrGid == 0 {
	//fi, err := os.Stat(n.path)
	//if err != nil {
	//	return err
	//}
	//st, ok := fi.Sys().(*syscall.Stat_t)
	//if !ok {
	//	return fmt.Errorf("unknown stat.Sys %T", fi.Sys())
	//}
	//if req.Valid&fuse.SetattrUid == 0 {
	//	req.Uid = st.Uid
	//} else {
	//	req.Gid = st.Gid
	//}
	//	}
	//	if err := os.Chown(n.path, int(req.Uid), int(req.Gid)); err != nil {
	//		return err
	//	}
	//}
	//if req.Valid&fuse.SetattrSize != 0 {
	//if err := os.Truncate(n.path, int64(req.Size)); err != nil {
	//	return err
	//}
	//log.Printf("Setattr %v size %v", f.Meta.Name, req.Size)
	//}

	//if req.Valid&fuse.SetattrAtime != 0 {
	//log.Printf("Setattr %v canot set atime", f.Meta.Name)
	//}
	//if req.Valid&fuse.SetattrMtime != 0 {
	//	log.Printf("Setattr %v cannot set mtime", f.Meta.Name)
	//}
	return nil
}

func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, res *fuse.OpenResponse) (fs.Handle, error) {
	f.log.Debug("OP Open")
	f.fs.updateLastOP()

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	f.state.openCount++
	f.fs.openFds++
	f.log.Debug("open count", "count", f.state.openCount, "global", f.fs.openFds)

	f.fs.cache[req.Header.Node] = struct{}{}
	f.log.Debug("current node cache", "cache", f.fs.cache)

	// Bypass page cache
	res.Flags |= fuse.OpenDirectIO

	// If it's the first file descriptor for this file, load the file content into a buffer so it can be written
	// FIXME(tsileo): instead of loading all the file in RAM, create a temporary file at $BLOBFS_WD/$PATH_IN_THE_FS
	// this way, if there's a power outage/unexpected exception, the WIP won't be loose (like is it right now)
	if f.state.openCount == 1 && len(f.meta.Refs) > 0 {
		f.log.Debug("Loading the file in memory")
		// if !f.fs.Immutable() && f.FakeFile == nil && f.data == nil {
		// f.log.Debug("Creating FakeFile")
		f.FakeFile = filereader.NewFile(f.fs.bs, f.meta)
		var err error
		f.data, err = ioutil.ReadAll(f.FakeFile)
		if err != nil {
			f.log.Error("failed to read", "err", err)
			return nil, err
		}
	}

	return f, nil
}

func (f *File) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	f.log.Debug("OP Release")
	f.fs.updateLastOP()

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	defer func() {
		f.state.openCount--
		f.fs.openFds--
		f.log.Debug("new openCount", "count", f.state.openCount, "global", f.fs.openFds)
		f.log.Debug("OP Release END")
	}()

	// If it's the last file descriptor for this file, then we need to save it
	if f.state.openCount == 1 {
		f.log.Debug("Last file descriptor for this node, cleaning up the FakeFile and data")
		if !f.fs.Immutable() && f.data != nil && len(f.data) > 0 && f.state.updated {
			f.meta.Size = len(f.data)
			// XXX(tsileo): data will be saved once the tree will be synced
			buf := bytes.NewBuffer(f.data)
			m2, err := f.fs.uploader.PutReader(f.meta.Name, &ClosingBuffer{buf})
			f.log.Debug("new meta", "meta", fmt.Sprintf("%+v", m2))
			// f.log.Debug("WriteResult", "wr", wr)
			if err != nil {
				return err
			}
			// f.parent.mu.Lock()
			// defer f.parent.mu.Unlock()
			f.meta = m2
			if err := f.parent.Save(); err != nil {
				return err
			}
			// f.log.Debug("new meta2", "meta", f.parent.Children[m2.Name].Meta(), "meta2", f.fs.root.Children[m2.Name].Meta())

			// f.log = f.log.New("ref", m2.Hash[:10])
			f.log.Debug("Flushed", "data_len", len(f.data))
			f.state.updated = false
		}
		// This is the last file descriptor, we can clean everything
		if f.FakeFile != nil {
			f.FakeFile.Close()
			f.FakeFile = nil
		}
		f.data = nil
	}
	return nil
}

func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	f.log.Debug("OP Fsync")
	f.fs.updateLastOP()
	// XXX(tsileo): flush the file?

	// This is a noop for now

	return nil
}

func (f *File) Read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	f.log.Debug("OP Read", "offset", req.Offset, "size", req.Size)
	f.fs.updateLastOP()

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	if f.data == nil && f.FakeFile == nil {
		f.log.Debug("Aborting, neither data or FakeFile is init")
		return nil
	}

	if req.Offset >= int64(f.Size()) {
		f.log.Debug("Aborting, out of boundaries offset")
		return nil
	}

	if f.fs.Immutable() {
		f.log.Debug("Reading from FakeFile")
		buf := make([]byte, req.Size)
		n, err := f.FakeFile.ReadAt(buf, req.Offset)
		if err == io.EOF {
			err = nil
		}
		if err != nil {
			return fuse.EIO
		}
		res.Data = buf[:n]
		return nil
	}

	f.log.Debug("Reading from memory")
	fuseutil.HandleRead(req, res, f.data)
	f.log.Debug("Resp len", "len", len(res.Data))
	return nil
}
