// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	cueast "cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
	cueparser "cuelang.org/go/cue/parser"

	"cuelang.org/go/cue/load"
	"cuelang.org/go/internal/cueimports"
	"cuelang.org/go/internal/golangorgx/gopls/cache/metadata"
	"cuelang.org/go/internal/golangorgx/gopls/cache/methodsets"
	"cuelang.org/go/internal/golangorgx/gopls/cache/typerefs"
	"cuelang.org/go/internal/golangorgx/gopls/cache/xrefs"
	"cuelang.org/go/internal/golangorgx/gopls/file"
	"cuelang.org/go/internal/golangorgx/gopls/protocol"
	"cuelang.org/go/internal/golangorgx/gopls/settings"
	"cuelang.org/go/internal/golangorgx/gopls/util/bug"
	"cuelang.org/go/internal/golangorgx/gopls/util/constraints"
	"cuelang.org/go/internal/golangorgx/gopls/util/immutable"
	"cuelang.org/go/internal/golangorgx/gopls/util/pathutil"
	"cuelang.org/go/internal/golangorgx/gopls/util/persistent"
	"cuelang.org/go/internal/golangorgx/tools/event"
	"cuelang.org/go/internal/golangorgx/tools/event/label"
	"cuelang.org/go/internal/golangorgx/tools/event/tag"
	"cuelang.org/go/internal/golangorgx/tools/memoize"
	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/go/types/objectpath"
)

// A Snapshot represents the current state for a given view.
//
// It is first and foremost an idempotent implementation of file.Source whose
// ReadFile method returns consistent information about the existence and
// content of each file throughout its lifetime.
//
// However, the snapshot also manages additional state (such as parsed files
// and packages) that are derived from file content.
//
// Snapshots are responsible for bookkeeping and invalidation of this state,
// implemented in Snapshot.clone.
type Snapshot struct {
	// sequenceID is the monotonically increasing ID of this snapshot within its View.
	//
	// Sequence IDs for Snapshots from different Views cannot be compared.
	sequenceID uint64

	// TODO(rfindley): the snapshot holding a reference to the view poses
	// lifecycle problems: a view may be shut down and waiting for work
	// associated with this snapshot to complete. While most accesses of the view
	// are benign (options or workspace information), this is not formalized and
	// it is wrong for the snapshot to use a shutdown view.
	//
	// Fix this by passing options and workspace information to the snapshot,
	// both of which should be immutable for the snapshot.
	view *View

	cancel        func()
	backgroundCtx context.Context

	store *memoize.Store // cache of handles shared by all snapshots

	refMu sync.Mutex

	// refcount holds the number of outstanding references to the current
	// Snapshot. When refcount is decremented to 0, the Snapshot maps are
	// destroyed and the done function is called.
	//
	// TODO(rfindley): use atomic.Int32 on Go 1.19+.
	refcount int
	done     func() // for implementing Session.Shutdown

	// mu guards all of the maps in the snapshot, as well as the builtin URI and
	// initialized.
	mu sync.Mutex

	// initialized reports whether the snapshot has been initialized. Concurrent
	// initialization is guarded by the view.initializationSema. Each snapshot is
	// initialized at most once: concurrent initialization is guarded by
	// view.initializationSema.
	initialized bool

	// initialErr holds the last error resulting from initialization. If
	// initialization fails, we only retry when the workspace modules change,
	// to avoid too many go/packages calls.
	// If initialized is false, initialErr stil holds the error resulting from
	// the previous initialization.
	// TODO(rfindley): can we unify the lifecycle of initialized and initialErr.
	initialErr *InitializationError

	// meta holds loaded metadata.
	//
	// meta is guarded by mu, but the Graph itself is immutable.
	//
	// TODO(rfindley): in many places we hold mu while operating on meta, even
	// though we only need to hold mu while reading the pointer.
	meta *metadata.Graph

	// files maps file URIs to their corresponding FileHandles.
	// It may invalidated when a file's content changes.
	files *fileMap

	// symbolizeHandles maps each file URI to a handle for the future
	// result of computing the symbols declared in that file.
	symbolizeHandles *persistent.Map[protocol.DocumentURI, *memoize.Promise] // *memoize.Promise[symbolizeResult]

	// packages maps a packageKey to a *packageHandle.
	// It may be invalidated when a file's content changes.
	//
	// Invariants to preserve:
	//  - packages.Get(id).meta == meta.metadata[id] for all ids
	//  - if a package is in packages, then all of its dependencies should also
	//    be in packages, unless there is a missing import
	packages *persistent.Map[ImportPath, *packageHandle]

	// activePackages maps a package path to a memoized active build
	// instance, or nil if the package is known not to be open.
	//
	// package paths not contained in the map are not known to be open
	// or not open.
	activePackages *persistent.Map[ImportPath, *build.Instance]

	// workspacePackages contains the workspace's packages, which are loaded
	// when the view is created. It does not contain intermediate test variants.
	workspacePackages immutable.Map[ImportPath, unit]

	// shouldLoad tracks packages that need to be reloaded, mapping a PackageID
	// to the package paths that should be used to reload it
	//
	// When we try to load a package, we clear it from the shouldLoad map
	// regardless of whether the load succeeded, to prevent endless loads.
	shouldLoad *persistent.Map[ImportPath, []PackagePath]

	// unloadableFiles keeps track of files that we've failed to load.
	unloadableFiles *persistent.Set[protocol.DocumentURI]

	// importGraph holds a shared import graph to use for type-checking. Adding
	// more packages to this import graph can speed up type checking, at the
	// expense of in-use memory.
	//
	// See getImportGraph for additional documentation.
	importGraphDone chan struct{} // closed when importGraph is set; may be nil
	importGraph     *importGraph  // copied from preceding snapshot and re-evaluated

	// pkgIndex is an index of package IDs, for efficient storage of typerefs.
	pkgIndex *typerefs.PackageIndex
}

var _ memoize.RefCounted = (*Snapshot)(nil) // snapshots are reference-counted

func (s *Snapshot) awaitPromise(ctx context.Context, p *memoize.Promise) (interface{}, error) {
	return p.Get(ctx, s)
}

// Acquire prevents the snapshot from being destroyed until the returned
// function is called.
//
// (s.Acquire().release() could instead be expressed as a pair of
// method calls s.IncRef(); s.DecRef(). The latter has the advantage
// that the DecRefs are fungible and don't require holding anything in
// addition to the refcounted object s, but paradoxically that is also
// an advantage of the current approach, which forces the caller to
// consider the release function at every stage, making a reference
// leak more obvious.)
func (s *Snapshot) Acquire() func() {
	s.refMu.Lock()
	defer s.refMu.Unlock()
	assert(s.refcount > 0, "non-positive refs")
	s.refcount++

	return s.decref
}

// decref should only be referenced by Acquire, and by View when it frees its
// reference to View.snapshot.
func (s *Snapshot) decref() {
	s.refMu.Lock()
	defer s.refMu.Unlock()

	assert(s.refcount > 0, "non-positive refs")
	s.refcount--
	if s.refcount == 0 {
		s.packages.Destroy()
		s.activePackages.Destroy()
		s.files.destroy()
		s.symbolizeHandles.Destroy()
		s.unloadableFiles.Destroy()
		s.done()
	}
}

// SequenceID is the sequence id of this snapshot within its containing
// view.
//
// Relative to their view sequence ids are monotonically increasing, but this
// does not hold globally: when new views are created their initial snapshot
// has sequence ID 0.
func (s *Snapshot) SequenceID() uint64 {
	return s.sequenceID
}

// SnapshotLabels returns a new slice of labels that should be used for events
// related to a snapshot.
func (s *Snapshot) Labels() []label.Label {
	return []label.Label{tag.Snapshot.Of(s.SequenceID()), tag.Directory.Of(s.Folder())}
}

// Folder returns the folder at the base of this snapshot.
func (s *Snapshot) Folder() protocol.DocumentURI {
	return s.view.folder.Dir
}

// View returns the View associated with this snapshot.
func (s *Snapshot) View() *View {
	return s.view
}

// FileKind returns the kind of a file.
//
// We can't reliably deduce the kind from the file name alone,
// as some editors can be told to interpret a buffer as
// language different from the file name heuristic, e.g. that
// an .html file actually contains Go "html/template" syntax,
// or even that a .go file contains Python.
func (s *Snapshot) FileKind(fh file.Handle) file.Kind {
	return fileKind(fh)
}

// fileKind returns the default file kind for a file, before considering
// template file extensions. See [Snapshot.FileKind].
func fileKind(fh file.Handle) file.Kind {
	// The kind of an unsaved buffer comes from the
	// TextDocumentItem.LanguageID field in the didChange event,
	// not from the file name. They may differ.
	if o, ok := fh.(*overlay); ok {
		if o.kind != file.UnknownKind {
			return o.kind
		}
	}

	fext := filepath.Ext(fh.URI().Path())
	switch fext {
	case ".go":
		return file.Go
	case ".mod":
		return file.Mod
	case ".sum":
		return file.Sum
	case ".work":
		return file.Work
	case ".cue":
		return file.CUE
	}
	return file.UnknownKind
}

// Options returns the options associated with this snapshot.
func (s *Snapshot) Options() *settings.Options {
	return s.view.folder.Options
}

// BackgroundContext returns a context used for all background processing
// on behalf of this snapshot.
func (s *Snapshot) BackgroundContext() context.Context {
	return s.backgroundCtx
}

// Templates returns the .tmpl files.
func (s *Snapshot) Templates() map[protocol.DocumentURI]file.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()

	tmpls := map[protocol.DocumentURI]file.Handle{}
	s.files.foreach(func(k protocol.DocumentURI, fh file.Handle) {
		if s.FileKind(fh) == file.Tmpl {
			tmpls[k] = fh
		}
	})
	return tmpls
}

// InvocationFlags represents the settings of a particular go command invocation.
// It is a mode, plus a set of flag bits.
type InvocationFlags int

const (
	// Normal is appropriate for commands that might be run by a user and don't
	// deliberately modify go.mod files, e.g. `go test`.
	Normal InvocationFlags = iota
	// WriteTemporaryModFile is for commands that need information from a
	// modified version of the user's go.mod file, e.g. `go mod tidy` used to
	// generate diagnostics.
	WriteTemporaryModFile
	// LoadWorkspace is for packages.Load, and other operations that should
	// consider the whole workspace at once.
	LoadWorkspace
	// AllowNetwork is a flag bit that indicates the invocation should be
	// allowed to access the network.
	AllowNetwork InvocationFlags = 1 << 10
)

func (m InvocationFlags) Mode() InvocationFlags {
	return m & (AllowNetwork - 1)
}

func (m InvocationFlags) AllowNetwork() bool {
	return m&AllowNetwork != 0
}

func (s *Snapshot) buildOverlay() map[string]load.Source {
	overlays := make(map[string]load.Source)
	for _, overlay := range s.Overlays() {
		if overlay.saved {
			continue
		}
		// TODO(rfindley): previously, there was a todo here to make sure we don't
		// send overlays outside of the current view. IMO we should instead make
		// sure this doesn't matter.
		overlays[overlay.URI().Path()] = load.FromBytes(overlay.content)
	}
	return overlays
}

// Overlays returns the set of overlays at this snapshot.
//
// Note that this may differ from the set of overlays on the server, if the
// snapshot observed a historical state.
func (s *Snapshot) Overlays() []*overlay {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.files.getOverlays()
}

// Package data kinds, identifying various package data that may be stored in
// the file cache.
const (
	xrefsKind       = "xrefs"
	methodSetsKind  = "methodsets"
	exportDataKind  = "export"
	diagnosticsKind = "diagnostics"
	typerefsKind    = "typerefs"
)

// PackageDiagnostics returns diagnostics for files contained in specified
// packages.
//
// If these diagnostics cannot be loaded from cache, the requested packages
// may be type-checked.
func (s *Snapshot) PackageDiagnostics(ctx context.Context, paths ...ImportPath) (map[protocol.DocumentURI][]*Diagnostic, error) {
	ctx, done := event.Start(ctx, "cache.snapshot.PackageDiagnostics")
	defer done()

	return make(map[protocol.DocumentURI][]*Diagnostic), nil
}

// References returns cross-reference indexes for the specified packages.
//
// If these indexes cannot be loaded from cache, the requested packages may
// be type-checked.
func (s *Snapshot) References(ctx context.Context, ids ...PackageID) ([]xrefIndex, error) {
	ctx, done := event.Start(ctx, "cache.snapshot.References")
	defer done()

	return nil, nil
}

// An xrefIndex is a helper for looking up references in a given package.
type xrefIndex struct {
	mp   *metadata.Package
	data []byte
}

func (index xrefIndex) Lookup(targets map[PackagePath]map[objectpath.Path]struct{}) []protocol.Location {
	return xrefs.Lookup(index.mp, index.data, targets)
}

// MethodSets returns method-set indexes for the specified packages.
//
// If these indexes cannot be loaded from cache, the requested packages may
// be type-checked.
func (s *Snapshot) MethodSets(ctx context.Context, ids ...PackageID) ([]*methodsets.Index, error) {
	ctx, done := event.Start(ctx, "cache.snapshot.MethodSets")
	defer done()

	return nil, nil
}

// MetadataForFile returns a new slice containing metadata for each
// package containing the Go file identified by uri, ordered by the
// number of CompiledGoFiles (i.e. "narrowest" to "widest" package),
// and secondarily by IsIntermediateTestVariant (false < true).
// The result may include tests and intermediate test variants of
// importable packages.
// It returns an error if the context was cancelled.
func (s *Snapshot) MetadataForFile(ctx context.Context, uri protocol.DocumentURI) ([]*build.Instance, error) {
	if s.view.typ == AdHocView {
		// As described in golang/go#57209, in ad-hoc workspaces (where we load ./
		// rather than ./...), preempting the directory load with file loads can
		// lead to an inconsistent outcome, where certain files are loaded with
		// command-line-arguments packages and others are loaded only in the ad-hoc
		// package. Therefore, ensure that the workspace is loaded before doing any
		// file loads.
		if err := s.awaitLoaded(ctx); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()

	// Start with the set of package associations derived from the last load.
	pkgPaths := s.meta.FilesToPackage[uri]

	shouldLoad := false
	for _, path := range pkgPaths {
		if pkgs, _ := s.shouldLoad.Get(path); len(pkgs) > 0 {
			shouldLoad = true
		}
	}

	// Check if uri is known to be unloadable.
	unloadable := s.unloadableFiles.Contains(uri)

	s.mu.Unlock()

	// Reload if loading is likely to improve the package associations for uri:
	//  - uri is not contained in any valid packages
	//  - ...or one of the packages containing uri is marked 'shouldLoad'
	//  - ...but uri is not unloadable
	if (shouldLoad || len(pkgPaths) == 0) && !unloadable {
		scope := fileLoadScope(uri)
		err := s.load(ctx, false, scope)

		//
		// Return the context error here as the current operation is no longer
		// valid.
		if err != nil {
			// Guard against failed loads due to context cancellation. We don't want
			// to mark loads as completed if they failed due to context cancellation.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			// Don't return an error here, as we may still return stale IDs.
			// Furthermore, the result of MetadataForFile should be consistent upon
			// subsequent calls, even if the file is marked as unloadable.
			if !errors.Is(err, errNoInstances) {
				event.Error(ctx, "MetadataForFile", err)
			}
		}

		// We must clear scopes after loading.
		//
		// TODO(rfindley): unlike reloadWorkspace, this is simply marking loaded
		// packages as loaded. We could do this from snapshot.load and avoid
		// raciness.
		s.clearShouldLoad(scope)
	}

	// Retrieve the metadata.
	s.mu.Lock()
	defer s.mu.Unlock()
	pkgPaths = s.meta.FilesToPackage[uri]
	metas := make([]*build.Instance, len(pkgPaths))
	for i, path := range pkgPaths {
		metas[i] = s.meta.Packages[path]
		if metas[i] == nil {
			panic("nil metadata")
		}
	}
	// Metadata is only ever added by loading,
	// so if we get here and still have
	// no IDs, uri is unloadable.
	if !unloadable && len(pkgPaths) == 0 {
		s.unloadableFiles.Add(uri)
	}

	// Sort packages "narrowest" to "widest" (in practice:
	// non-tests before tests), and regular packages before
	// their intermediate test variants (which have the same
	// files but different imports).
	sort.Slice(metas, func(i, j int) bool {
		x, y := metas[i], metas[j]
		xfiles, yfiles := len(x.BuildFiles), len(y.BuildFiles)
		return xfiles < yfiles
	})

	return metas, nil
}

// -- Active package tracking --
//
// We say a package is "active" if any of its files are open.
// This is an optimization: the "active" concept is an
// implementation detail of the cache and is not exposed
// in the source or Snapshot API.
// After type-checking we keep active packages in memory.
// The activePackages persistent map does bookkeeping for
// the set of active packages.

// getActivePackage returns a the memoized active package for path, if
// it exists.  If path is not active or has not yet been type-checked,
// it returns nil.
func (s *Snapshot) getActivePackage(path ImportPath) *build.Instance {
	s.mu.Lock()
	defer s.mu.Unlock()

	if value, ok := s.activePackages.Get(path); ok {
		return value
	}
	return nil
}

// setActivePackage checks if inst is active, and if so either records it in
// the active packages map or returns the existing memoized active package for path.
func (s *Snapshot) setActivePackage(path ImportPath, inst *build.Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.activePackages.Get(path); ok {
		return // already memoized
	}

	if containsOpenFileLocked(s, inst) {
		s.activePackages.Set(path, inst, nil)
	} else {
		s.activePackages.Set(path, nil, nil) // remember that pkg is not open
	}
}

func (s *Snapshot) resetActivePackagesLocked() {
	s.activePackages.Destroy()
	s.activePackages = new(persistent.Map[ImportPath, *build.Instance])
}

// See Session.FileWatchingGlobPatterns for a description of gopls' file
// watching heuristic.
func (s *Snapshot) fileWatchingGlobPatterns() map[protocol.RelativePattern]unit {
	// Always watch files that may change the view definition.
	patterns := make(map[protocol.RelativePattern]unit)

	// If GOWORK is outside the folder, ensure we are watching it.

	extensions := "cue"
	watchCueFiles := fmt.Sprintf("**/*.{%s}", extensions)

	var dirs []string
	if s.view.moduleMode() {

		// In module mode, watch directories containing active modules, and collect
		// these dirs for later filtering the set of known directories.
		//
		// The assumption is that the user is not actively editing non-workspace
		// modules, so don't pay the price of file watching.
		for modFile := range s.view.workspaceModFiles {
			rootDir := filepath.Dir(filepath.Dir(modFile.Path()))
			dirs = append(dirs, rootDir)

			// TODO(golang/go#64724): thoroughly test these patterns, particularly on
			// on Windows.
			//
			// Note that glob patterns should use '/' on Windows:
			// https://code.visualstudio.com/docs/editor/glob-patterns
			patterns[protocol.RelativePattern{BaseURI: protocol.URIFromPath(rootDir), Pattern: watchCueFiles}] = unit{}
		}
	} else {
		// In non-module modes (GOPATH or AdHoc), we just watch the workspace root.
		dirs = []string{s.view.root.Path()}
		patterns[protocol.RelativePattern{Pattern: watchCueFiles}] = unit{}
	}

	if s.watchSubdirs() {
		// Some clients (e.g. VS Code) do not send notifications for changes to
		// directories that contain Go code (golang/go#42348). To handle this,
		// explicitly watch all of the directories in the workspace. We find them
		// by adding the directories of every file in the snapshot's workspace
		// directories. There may be thousands of patterns, each a single
		// directory.
		//
		// We compute this set by looking at files that we've previously observed.
		// This may miss changed to directories that we haven't observed, but that
		// shouldn't matter as there is nothing to invalidate (if a directory falls
		// in forest, etc).
		//
		// (A previous iteration created a single glob pattern holding a union of
		// all the directories, but this was found to cause VS Code to get stuck
		// for several minutes after a buffer was saved twice in a workspace that
		// had >8000 watched directories.)
		//
		// Some clients (notably coc.nvim, which uses watchman for globs) perform
		// poorly with a large list of individual directories.
		s.addKnownSubdirs(patterns, dirs)
	}

	return patterns
}

func (s *Snapshot) addKnownSubdirs(patterns map[protocol.RelativePattern]unit, wsDirs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.files.getDirs().Range(func(dir string) {
		for _, wsDir := range wsDirs {
			if pathutil.InDir(wsDir, dir) {
				patterns[protocol.RelativePattern{Pattern: filepath.ToSlash(dir)}] = unit{}
			}
		}
	})
}

// watchSubdirs reports whether gopls should request separate file watchers for
// each relevant subdirectory. This is necessary only for clients (namely VS
// Code) that do not send notifications for individual files in a directory
// when the entire directory is deleted.
func (s *Snapshot) watchSubdirs() bool {
	switch p := s.Options().SubdirWatchPatterns; p {
	case settings.SubdirWatchPatternsOn:
		return true
	case settings.SubdirWatchPatternsOff:
		return false
	case settings.SubdirWatchPatternsAuto:
		// See the documentation of InternalOptions.SubdirWatchPatterns for an
		// explanation of why VS Code gets a different default value here.
		//
		// Unfortunately, there is no authoritative list of client names, nor any
		// requirements that client names do not change. We should update the VS
		// Code extension to set a default value of "subdirWatchPatterns" to "on",
		// so that this workaround is only temporary.
		if s.Options().ClientInfo != nil && s.Options().ClientInfo.Name == "Visual Studio Code" {
			return true
		}
		return false
	default:
		bug.Reportf("invalid subdirWatchPatterns: %q", p)
		return false
	}
}

// filesInDir returns all files observed by the snapshot that are contained in
// a directory with the provided URI.
func (s *Snapshot) filesInDir(uri protocol.DocumentURI) []protocol.DocumentURI {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := uri.Path()
	if !s.files.getDirs().Contains(dir) {
		return nil
	}
	var files []protocol.DocumentURI
	s.files.foreach(func(uri protocol.DocumentURI, _ file.Handle) {
		if pathutil.InDir(dir, uri.Path()) {
			files = append(files, uri)
		}
	})
	return files
}

// WorkspaceMetadata returns a new, unordered slice containing
// metadata for all ordinary and test packages (but not
// intermediate test variants) in the workspace.
//
// The workspace is the set of modules typically defined by a
// go.work file. It is not transitively closed: for example,
// the standard library is not usually part of the workspace
// even though every module in the workspace depends on it.
//
// Operations that must inspect all the dependencies of the
// workspace packages should instead use AllMetadata.
func (s *Snapshot) WorkspaceMetadata(ctx context.Context) ([]*build.Instance, error) {
	if err := s.awaitLoaded(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	meta := make([]*build.Instance, 0, s.workspacePackages.Len())
	s.workspacePackages.Range(func(path ImportPath, _ unit) {
		meta = append(meta, s.meta.Packages[path])
	})
	return meta, nil
}

// isWorkspacePackage reports whether the given package ID refers to a
// workspace package for the snapshot.
func (s *Snapshot) isWorkspacePackage(path ImportPath) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.workspacePackages.Value(path)
	return ok
}

// Symbols extracts and returns symbol information for every file contained in
// a loaded package. It awaits snapshot loading.
//
// If workspaceOnly is set, this only includes symbols from files in a
// workspace package. Otherwise, it returns symbols from all loaded packages.
//
// TODO(rfindley): move to symbols.go.
func (s *Snapshot) Symbols(ctx context.Context, workspaceOnly bool) (map[protocol.DocumentURI][]Symbol, error) {
	var (
		meta []*build.Instance
		err  error
	)
	if workspaceOnly {
		meta, err = s.WorkspaceMetadata(ctx)
	} else {
		meta, err = s.AllMetadata(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("loading metadata: %v", err)
	}

	buildFiles := make(map[protocol.DocumentURI]struct{})
	for _, inst := range meta {
		for _, file := range inst.BuildFiles {
			buildFiles[protocol.URIFromPath(file.Filename)] = struct{}{}
		}
	}

	// Symbolize them in parallel.
	var (
		group    errgroup.Group
		nprocs   = 2 * runtime.GOMAXPROCS(-1) // symbolize is a mix of I/O and CPU
		resultMu sync.Mutex
		result   = make(map[protocol.DocumentURI][]Symbol)
	)
	group.SetLimit(nprocs)
	for uri := range buildFiles {
		uri := uri
		group.Go(func() error {
			symbols, err := s.symbolize(ctx, uri)
			if err != nil {
				return err
			}
			resultMu.Lock()
			result[uri] = symbols
			resultMu.Unlock()
			return nil
		})
	}
	// Keep going on errors, but log the first failure.
	// Partial results are better than no symbol results.
	if err := group.Wait(); err != nil {
		event.Error(ctx, "getting snapshot symbols", err)
	}
	return result, nil
}

// AllMetadata returns a new unordered array of metadata for
// all packages known to this snapshot, which includes the
// packages of all workspace modules plus their transitive
// import dependencies.
//
// It may also contain ad-hoc packages for standalone files.
// It includes all test variants.
//
// TODO(rfindley): Replace this with s.MetadataGraph().
func (s *Snapshot) AllMetadata(ctx context.Context) ([]*build.Instance, error) {
	if err := s.awaitLoaded(ctx); err != nil {
		return nil, err
	}

	g := s.MetadataGraph()

	meta := make([]*build.Instance, 0, len(g.Packages))
	for _, mp := range g.Packages {
		meta = append(meta, mp)
	}
	return meta, nil
}

// CueModForFile returns the URI of the go.mod file for the given URI.
//
// TODO(rfindley): clarify that this is only active modules. Or update to just
// use findRootPattern.
func (s *Snapshot) CueModForFile(uri protocol.DocumentURI) protocol.DocumentURI {
	return moduleForURI(s.view.workspaceModFiles, uri)
}

func moduleForURI(modFiles map[protocol.DocumentURI]struct{}, uri protocol.DocumentURI) protocol.DocumentURI {
	var match protocol.DocumentURI
	for modURI := range modFiles {
		if !modURI.Dir().Encloses(uri) {
			continue
		}
		if len(modURI) > len(match) {
			match = modURI
		}
	}
	return match
}

// Metadata returns the metadata for the specified package,
// or nil if it was not found.
func (s *Snapshot) Metadata(path ImportPath) *build.Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta.Packages[path]
}

// clearShouldLoad clears package IDs that no longer need to be reloaded after
// scopes has been loaded.
func (s *Snapshot) clearShouldLoad(scopes ...loadScope) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, scope := range scopes {
		switch scope := scope.(type) {
		case packageLoadScope:
			scopePath := PackagePath(scope)
			var toDelete []ImportPath
			s.shouldLoad.Range(func(path ImportPath, pkgPaths []PackagePath) {
				for _, pkgPath := range pkgPaths {
					if pkgPath == scopePath {
						toDelete = append(toDelete, path)
					}
				}
			})
			for _, id := range toDelete {
				s.shouldLoad.Delete(id)
			}
		case fileLoadScope:
			uri := protocol.DocumentURI(scope)
			pkgPaths := s.meta.FilesToPackage[uri]
			for _, path := range pkgPaths {
				s.shouldLoad.Delete(path)
			}
		}
	}
}

// FindFile returns the FileHandle for the given URI, if it is already
// in the given snapshot.
// TODO(adonovan): delete this operation; use ReadFile instead.
func (s *Snapshot) FindFile(uri protocol.DocumentURI) file.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, _ := s.files.get(uri)
	return result
}

// ReadFile returns a File for the given URI. If the file is unknown it is added
// to the managed set.
//
// ReadFile succeeds even if the file does not exist. A non-nil error return
// indicates some type of internal error, for example if ctx is cancelled.
func (s *Snapshot) ReadFile(ctx context.Context, uri protocol.DocumentURI) (file.Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fh, ok := s.files.get(uri)
	if !ok {
		var err error
		fh, err = s.view.fs.ReadFile(ctx, uri)
		if err != nil {
			return nil, err
		}
		s.files.set(uri, fh)
	}
	return fh, nil
}

// preloadFiles delegates to the view FileSource to read the requested uris in
// parallel, without holding the snapshot lock.
func (s *Snapshot) preloadFiles(ctx context.Context, uris []protocol.DocumentURI) {
	files := make([]file.Handle, len(uris))
	var wg sync.WaitGroup
	iolimit := make(chan struct{}, 20) // I/O concurrency limiting semaphore
	for i, uri := range uris {
		wg.Add(1)
		iolimit <- struct{}{}
		go func(i int, uri protocol.DocumentURI) {
			defer wg.Done()
			fh, err := s.view.fs.ReadFile(ctx, uri)
			<-iolimit
			if err != nil && ctx.Err() == nil {
				event.Error(ctx, fmt.Sprintf("reading %s", uri), err)
				return
			}
			files[i] = fh
		}(i, uri)
	}
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	for i, fh := range files {
		if fh == nil {
			continue // error logged above
		}
		uri := uris[i]
		if _, ok := s.files.get(uri); !ok {
			s.files.set(uri, fh)
		}
	}
}

// IsOpen returns whether the editor currently has a file open.
func (s *Snapshot) IsOpen(uri protocol.DocumentURI) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	fh, _ := s.files.get(uri)
	_, open := fh.(*overlay)
	return open
}

// MetadataGraph returns the current metadata graph for the Snapshot.
func (s *Snapshot) MetadataGraph() *metadata.Graph {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// InitializationError returns the last error from initialization.
func (s *Snapshot) InitializationError() *InitializationError {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialErr
}

// awaitLoaded awaits initialization and package reloading, and returns
// ctx.Err().
func (s *Snapshot) awaitLoaded(ctx context.Context) error {
	// Do not return results until the snapshot's view has been initialized.
	s.AwaitInitialized(ctx)
	s.reloadWorkspace(ctx)
	return ctx.Err()
}

// AwaitInitialized waits until the snapshot's view is initialized.
func (s *Snapshot) AwaitInitialized(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-s.view.initialWorkspaceLoad:
	}
	// We typically prefer to run something as intensive as the IWL without
	// blocking. I'm not sure if there is a way to do that here.
	s.initialize(ctx, false)
}

// reloadWorkspace reloads the metadata for all invalidated workspace packages.
func (s *Snapshot) reloadWorkspace(ctx context.Context) {
	var scopes []loadScope
	var seen map[PackagePath]bool
	s.mu.Lock()
	s.shouldLoad.Range(func(_ ImportPath, pkgPaths []PackagePath) {
		for _, pkgPath := range pkgPaths {
			if seen == nil {
				seen = make(map[PackagePath]bool)
			}
			if seen[pkgPath] {
				continue
			}
			seen[pkgPath] = true
			scopes = append(scopes, packageLoadScope(pkgPath))
		}
	})
	s.mu.Unlock()

	if len(scopes) == 0 {
		return
	}

	// For an ad-hoc view, we cannot reload by package path. Just reload the view.
	if s.view.typ == AdHocView {
		scopes = []loadScope{viewLoadScope{}}
	}

	err := s.load(ctx, false, scopes...)

	// Unless the context was canceled, set "shouldLoad" to false for all
	// of the metadata we attempted to load.
	if !errors.Is(err, context.Canceled) {
		s.clearShouldLoad(scopes...)
		if err != nil {
			event.Error(ctx, "reloading workspace", err, s.Labels()...)
		}
	}
}

// orphanedFileDiagnosticRange returns the position to use for orphaned file diagnostics.
// We only warn about an orphaned file if it is well-formed enough to actually
// be part of a package. Otherwise, we need more information.
func orphanedFileDiagnosticRange(ctx context.Context, cache *parseCache, fh file.Handle) (*ParsedGoFile, protocol.Range, bool) {
	pgfs, err := cache.parseFiles(ctx, token.NewFileSet(), ParseHeader, false, fh)
	if err != nil {
		return nil, protocol.Range{}, false
	}
	pgf := pgfs[0]
	if !pgf.File.Name.Pos().IsValid() {
		return nil, protocol.Range{}, false
	}
	rng, err := pgf.PosRange(pgf.File.Name.Pos(), pgf.File.Name.End())
	if err != nil {
		return nil, protocol.Range{}, false
	}
	return pgf, rng, true
}

// TODO(golang/go#53756): this function needs to consider more than just the
// absolute URI, for example:
//   - the position of /vendor/ with respect to the relevant module root
//   - whether or not go.work is in use (as vendoring isn't supported in workspace mode)
//
// Most likely, each call site of inVendor needs to be reconsidered to
// understand and correctly implement the desired behavior.
func inVendor(uri protocol.DocumentURI) bool {
	_, after, found := strings.Cut(string(uri), "/vendor/")
	// Only subdirectories of /vendor/ are considered vendored
	// (/vendor/a/foo.go is vendored, /vendor/foo.go is not).
	return found && strings.Contains(after, "/")
}

// This exists temporarily to support development and debugging
//
// TODO(ms): delete asap
func printStackTrace(prefix string) {
	buf := make([]byte, 16384)
	n := runtime.Stack(buf, false) // false = only current goroutine
	fmt.Printf("%s\n%s\n\n", prefix, string(buf[:n]))
}

// clone copies state from the receiver into a new Snapshot, applying the given
// state changes.
//
// The caller of clone must call Snapshot.decref on the returned
// snapshot when they are finished using it.
//
// The resulting bool reports whether the change invalidates any derived
// diagnostics for the snapshot, for example because it invalidates Packages or
// parsed go.mod files. This is used to mark a view as needing diagnosis in the
// server.
//
// TODO(rfindley): long term, it may be better to move responsibility for
// diagnostics into the Snapshot (e.g. a Snapshot.Diagnostics method), at which
// point the Snapshot could be responsible for tracking and forwarding a
// 'viewsToDiagnose' field. As is, this field is instead externalized in the
// server.viewsToDiagnose map. Moving it to the snapshot would entirely
// eliminate any 'relevance' heuristics from Session.DidModifyFiles, but would
// also require more strictness about diagnostic dependencies. For example,
// template.Diagnostics currently re-parses every time: there is no Snapshot
// data responsible for providing these diagnostics.
func (s *Snapshot) clone(ctx, bgCtx context.Context, changed StateChange, done func()) (*Snapshot, bool) {
	changedFiles := changed.Files
	ctx, stop := event.Start(ctx, "cache.snapshot.clone")
	defer stop()

	s.mu.Lock()
	defer s.mu.Unlock()

	needsDiagnosis := false

	bgCtx, cancel := context.WithCancel(bgCtx)
	result := &Snapshot{
		sequenceID:        s.sequenceID + 1,
		view:              s.view,
		cancel:            cancel,
		backgroundCtx:     bgCtx,
		store:             s.store,
		refcount:          1, // Snapshots are born referenced.
		done:              done,
		initialized:       s.initialized,
		initialErr:        s.initialErr,
		files:             s.files.clone(changedFiles),
		symbolizeHandles:  cloneWithout(s.symbolizeHandles, changedFiles, nil),
		packages:          s.packages.Clone(),
		activePackages:    s.activePackages.Clone(),
		workspacePackages: s.workspacePackages,
		shouldLoad:        s.shouldLoad.Clone(),      // not cloneWithout: shouldLoad is cleared on loads
		unloadableFiles:   s.unloadableFiles.Clone(), // not cloneWithout: typing in a file doesn't necessarily make it loadable
		importGraph:       s.importGraph,
		pkgIndex:          s.pkgIndex,
	}

	reinit := false

	// Collect observed file handles for changed URIs from the old snapshot, if
	// they exist. Importantly, we don't call ReadFile here: consider the case
	// where a file is added on disk; we don't want to read the newly added file
	// into the old snapshot, as that will break our change detection below.
	//
	// TODO(rfindley): it may be more accurate to rely on the
	// modification type here, similarly to what we do for vendored
	// files above. If we happened not to have read a file in the
	// previous snapshot, that's not the same as it actually being
	// created.
	//
	// TODO(ms): (see the go-tools equiv for "vendored files above").
	oldFiles := make(map[protocol.DocumentURI]file.Handle)
	for uri := range changedFiles {
		if fh, ok := s.files.get(uri); ok {
			oldFiles[uri] = fh
		}
	}
	// changedOnDisk determines if the new file handle may have changed on disk.
	// It over-approximates, returning true if the new file is saved and either
	// the old file wasn't saved, or the on-disk contents changed.
	//
	// oldFH may be nil.
	changedOnDisk := func(oldFH, newFH file.Handle) bool {
		if !newFH.SameContentsOnDisk() {
			return false
		}
		if oe, ne := (oldFH != nil && fileExists(oldFH)), fileExists(newFH); !oe || !ne {
			return oe != ne
		}
		return !oldFH.SameContentsOnDisk() || oldFH.Identity() != newFH.Identity()
	}

	// Reinitialize if any workspace mod file has changed on disk.
	for uri, newFH := range changedFiles {
		if _, ok := result.view.workspaceModFiles[uri]; ok && changedOnDisk(oldFiles[uri], newFH) {
			reinit = true
		}
	}

	if reinit {
		result.initialized = false
	}

	// directPkgPaths keeps track of packages that have directly
	// changed.  Note: this is not a set, it's a map from id to
	// invalidateMetadata.
	directPkgPaths := map[ImportPath]bool{}

	// Invalidate all package metadata if the workspace module has changed.
	if reinit {
		for path := range s.meta.Packages {
			// TODO(rfindley): this seems brittle; can we just start over?
			directPkgPaths[path] = true
		}
	}

	// Compute invalidations based on file changes.
	anyImportDeleted := false      // import deletions can resolve cycles
	anyFileOpenedOrClosed := false // opened files affect workspace packages
	anyFileAdded := false          // adding a file can resolve missing dependencies

	for uri, newFH := range changedFiles {
		// The original FileHandle for this URI is cached on the snapshot.
		oldFH := oldFiles[uri] // may be nil
		_, oldOpen := oldFH.(*overlay)
		_, newOpen := newFH.(*overlay)

		anyFileOpenedOrClosed = anyFileOpenedOrClosed || (oldOpen != newOpen)
		anyFileAdded = anyFileAdded || (oldFH == nil || !fileExists(oldFH)) && fileExists(newFH)

		// If uri is a cue file, check if it has changed in a way that would
		// invalidate metadata.
		var invalidateMetadata, pkgFileChanged, importDeleted bool
		var pkgName string
		if s.FileKind(newFH) == file.CUE {
			invalidateMetadata, pkgFileChanged, importDeleted, pkgName = metadataChanges(ctx, s, oldFH, newFH)
		}
		if invalidateMetadata {
			// If this is a metadata-affecting change, perhaps a reload will succeed.
			result.unloadableFiles.Remove(uri)
			needsDiagnosis = true
		}

		invalidateMetadata = invalidateMetadata || reinit
		anyImportDeleted = anyImportDeleted || importDeleted

		pkgPaths := invalidatedPackageIDs(uri, s.meta, pkgFileChanged, pkgName)
		for path := range pkgPaths {
			directPkgPaths[path] = directPkgPaths[path] || invalidateMetadata // may insert 'false'
		}
	}

	// Deleting an import can cause list errors due to import cycles to be
	// resolved. The best we can do without parsing the list error message is to
	// hope that list errors may have been resolved by a deleted import.
	//
	// We could do better by parsing the list error message. We already do this
	// to assign a better range to the list error, but for such critical
	// functionality as metadata, it's better to be conservative until it proves
	// impractical.
	//
	// We could also do better by looking at which imports were deleted and
	// trying to find cycles they are involved in. This fails when the file goes
	// from an unparseable state to a parseable state, as we don't have a
	// starting point to compare with.
	//
	// TODO(ms): tidy comment above given we aren't using go list, and
	// figure out whether the following is sufficient.
	if anyImportDeleted {
		for path, inst := range s.meta.Packages {
			if inst.Err != nil {
				directPkgPaths[path] = true
			}
		}
	}

	// Adding a file can resolve missing dependencies from existing packages.
	//
	// We could be smart here and try to guess which packages may have been
	// fixed, but until that proves necessary, just invalidate metadata for any
	// package with missing dependencies.
	//
	// TODO(ms): this one needs more thinking about: I currently have
	// no idea what cue/load.Instances does with imports that can't be
	// resolved.
	//
	// if anyFileAdded {
	// 	for id, mp := range s.meta.Packages {
	// 		for _, impID := range mp.DepsByImpPath {
	// 			if impID == "" { // missing import
	// 				directIDs[id] = true
	// 				break
	// 			}
	// 		}
	// 	}
	// }

	// Invalidate reverse dependencies too.
	// pkgPathsToInvalidate keeps track of transitive reverse dependencies.
	// If a pkgPath is present in the map, invalidate its types.
	// If a pkgPath's value is true, invalidate its metadata too.
	pkgPathsToInvalidate := map[ImportPath]bool{}
	var addRevDeps func(ImportPath, bool)
	addRevDeps = func(path ImportPath, invalidateMetadata bool) {
		current, seen := pkgPathsToInvalidate[path]
		newInvalidateMetadata := current || invalidateMetadata

		// If we've already seen this path, and the value of invalidate
		// metadata has not changed, we can return early.
		if seen && current == newInvalidateMetadata {
			return
		}
		pkgPathsToInvalidate[path] = newInvalidateMetadata
		for _, importingPkgPath := range s.meta.ImportedBy[path] {
			addRevDeps(importingPkgPath, invalidateMetadata)
		}
	}
	for path, invalidateMetadata := range directPkgPaths {
		addRevDeps(path, invalidateMetadata)
	}

	// Invalidated package information.
	for path, invalidateMetadata := range pkgPathsToInvalidate {
		if _, ok := directPkgPaths[path]; ok || invalidateMetadata {
			if result.packages.Delete(path) {
				needsDiagnosis = true
			}
		} else {
			if entry, hit := result.packages.Get(path); hit {
				needsDiagnosis = true
				ph := entry.clone(false)
				result.packages.Set(path, ph, nil)
			}
		}
		if result.activePackages.Delete(path) {
			needsDiagnosis = true
		}
	}

	// Compute which metadata updates are required. We only need to invalidate
	// packages directly containing the affected file, and only if it changed in
	// a relevant way.
	metadataUpdates := make(map[ImportPath]*build.Instance)
	for path, inst := range s.meta.Packages {
		invalidateMetadata := pkgPathsToInvalidate[path]

		// For metadata that has been newly invalidated, capture package paths
		// requiring reloading in the shouldLoad map.
		//
		// TODO(ms): far from clear what the values of the shouldLoad
		// map should actually be - they seem to be related to
		// packageLoad scope, which we don't support (or understand).

		if invalidateMetadata && !metadata.IsCommandLineArguments(path) {
			needsReload := []PackagePath{PackagePath(inst.Dir)}
			result.shouldLoad.Set(path, needsReload, nil)
		}

		// Check whether the metadata should be deleted.
		if invalidateMetadata {
			needsDiagnosis = true
			metadataUpdates[path] = nil
		}
	}

	// Update metadata, if necessary.
	result.meta = s.meta.Update(metadataUpdates)

	// Update workspace and active packages, if necessary.
	if result.meta != s.meta || anyFileOpenedOrClosed {
		needsDiagnosis = true
		result.workspacePackages = computeWorkspacePackagesLocked(result, result.meta)
		result.resetActivePackagesLocked()
	} else {
		result.workspacePackages = s.workspacePackages
	}

	return result, needsDiagnosis
}

// cloneWithout clones m then deletes from it the keys of changes.
//
// The optional didDelete variable is set to true if there were deletions.
func cloneWithout[K constraints.Ordered, V1, V2 any](m *persistent.Map[K, V1], changes map[K]V2, didDelete *bool) *persistent.Map[K, V1] {
	m2 := m.Clone()
	for k := range changes {
		if m2.Delete(k) && didDelete != nil {
			*didDelete = true
		}
	}
	return m2
}

// cloneWith clones m then inserts the changes into it.
func cloneWith[K constraints.Ordered, V any](m *persistent.Map[K, V], changes map[K]V) *persistent.Map[K, V] {
	m2 := m.Clone()
	for k, v := range changes {
		m2.Set(k, v, nil)
	}
	return m2
}

// deleteMostRelevantModFile deletes the mod file most likely to be the mod
// file for the changed URI, if it exists.
//
// Specifically, this is the longest mod file path in a directory containing
// changed. This might not be accurate if there is another mod file closer to
// changed that happens not to be present in the map, but that's OK: the goal
// of this function is to guarantee that IF the nearest mod file is present in
// the map, it is invalidated.
func deleteMostRelevantModFile(m *persistent.Map[protocol.DocumentURI, *memoize.Promise], changed protocol.DocumentURI) {
	var mostRelevant protocol.DocumentURI
	changedFile := changed.Path()

	m.Range(func(modURI protocol.DocumentURI, _ *memoize.Promise) {
		if len(modURI) > len(mostRelevant) {
			if pathutil.InDir(filepath.Dir(modURI.Path()), changedFile) {
				mostRelevant = modURI
			}
		}
	})
	if mostRelevant != "" {
		m.Delete(mostRelevant)
	}
}

// invalidatedPackageIDs returns all packages invalidated by a change to uri.
//
// If packageFileChanged is set, the file is either a new file, or has a new
// package name. In this case, all known packages in the directory will be
// invalidated.
func invalidatedPackageIDs(uri protocol.DocumentURI, graph *metadata.Graph, packageFileChanged bool, pkgName string) map[ImportPath]struct{} {
	invalidated := make(map[ImportPath]struct{})

	// The only instances the file (uri) can be a part of are instances
	// from the same dir as the file, or descendants of the dir (with
	// the same package name). We want to find the "root" of these
	// instances, i.e. the instance with the some Dir as the file.
	var rootInvalidInstance *build.Instance

	// At a minimum, we invalidate packages known to contain uri.
	for _, pkgPath := range graph.FilesToPackage[uri] {
		invalidated[pkgPath] = struct{}{}
		invalidInst := graph.Packages[pkgPath]
		if invalidInst.PkgName != "_" && invalidInst.Dir == filepath.Dir(uri.Path()) {
			rootInvalidInstance = invalidInst
		}
	}

	// If the file didn't move to a new package (or the file has no
	// useful pkgName), we should only invalidate the packages it is
	// currently contained inside.
	if pkgName == "" || pkgName == "_" || rootInvalidInstance == nil || (!packageFileChanged && len(invalidated) > 0) {
		return invalidated
	}

	invalidIpt := cueast.ParseImportPath(rootInvalidInstance.ImportPath)

	// For all the packages we know about, add to the set of
	// invalidated packages, if they're in the same package (name) as
	// our file, and their import path is equal, or longer than our
	// root instance. This is the scenario when a file moves into a
	// package that already exists and may need to be included as an
	// ancestor.
	for pkgPath, inst := range graph.Packages {
		if _, invalid := invalidated[pkgPath]; invalid {
			continue
		}
		if inst.Module != rootInvalidInstance.Module {
			continue
		}
		if !strings.HasPrefix(inst.Dir, rootInvalidInstance.Dir) {
			continue
		}

		ipt := cueast.ParseImportPath(inst.ImportPath)

		if ipt.Qualifier == pkgName && strings.HasPrefix(ipt.Path, invalidIpt.Path+"/") {
			// It's the same pkgName (that we're moving to), and this inst is:
			// 1. in the same module,
			// 2. a descendant of the uri's directory, and
			// 3. a "descendant" of the rootInvalidInstance's package path
			// Therefore we invalidate it too. Note we deliberately
			// ignore versions and "explicit qualifier" here.
			invalidated[ImportPath(inst.ImportPath)] = struct{}{}
			break
		}
	}

	return invalidated
}

// fileWasSaved reports whether the FileHandle passed in has been saved. It
// accomplishes this by checking to see if the original and current FileHandles
// are both overlays, and if the current FileHandle is saved while the original
// FileHandle was not saved.
func fileWasSaved(originalFH, currentFH file.Handle) bool {
	c, ok := currentFH.(*overlay)
	if !ok || c == nil {
		return true
	}
	o, ok := originalFH.(*overlay)
	if !ok || o == nil {
		return c.saved
	}
	return !o.saved && c.saved
}

func readCueHeaders(fh file.Handle) (*cueast.File, error) {
	content, err := fh.Content()
	if err != nil {
		return nil, err
	}
	// make sure we only parse the package name and imports, and don't
	// spend time parsing the rest of the file.
	headers, err := cueimports.Read(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	return cueparser.ParseFile(fh.URI().Path(), headers, cueparser.ImportsOnly)
}

// metadataChanges detects features of the change from oldFH->newFH that may
// affect package metadata.
//
// It uses lockedSnapshot to access cached parse information. lockedSnapshot
// must be locked.
//
// The result parameters have the following meaning:
//   - invalidate means that package metadata for packages containing the file
//     should be invalidated.
//   - pkgFileChanged means that the file->package associates for the file have
//     changed (possibly because the file is new, or because its package name has
//     changed).
//   - importDeleted means that an import has been deleted, or we can't
//     determine if an import was deleted due to errors.
func metadataChanges(ctx context.Context, lockedSnapshot *Snapshot, oldFH, newFH file.Handle) (invalidate, pkgFileChanged, importDeleted bool, pkgName string) {
	oldExists := oldFH != nil && fileExists(oldFH)
	newExists := fileExists(newFH)

	var newHead *cueast.File
	var newErr error
	if newExists {
		newHead, newErr = readCueHeaders(newFH)
		if newErr == nil {
			pkgName = newHead.PackageName()
		}
	}

	if !oldExists || !newExists { // existential changes
		changed := oldExists != newExists
		return changed, changed, !newExists, pkgName // we don't know if an import was deleted
	}

	// If the file hasn't changed, there's no need to reload.
	if oldFH.Identity() == newFH.Identity() {
		return false, false, false, pkgName
	}

	// Parse headers to compare package names and imports.
	oldHead, oldErr := readCueHeaders(oldFH)

	if oldErr != nil || newErr != nil {
		errChanged := (oldErr == nil) != (newErr == nil)
		return errChanged, errChanged, errChanged, pkgName
	}

	// If a package name has changed, the set of package imports may have changed
	// in ways we can't detect here. Assume an import has been deleted.
	if oldHead.PackageName() != pkgName {
		return true, true, true, pkgName
	}

	// Check whether package imports have changed. Only consider potentially
	// valid imports paths.
	oldImports := validImports(oldHead.Imports)
	newImports := validImports(newHead.Imports)

	for path := range newImports {
		if _, ok := oldImports[path]; ok {
			delete(oldImports, path)
		} else {
			invalidate = true // a new, potentially valid import was added
		}
	}

	if len(oldImports) > 0 {
		invalidate = true
		importDeleted = true
	}

	// TODO(ms): in the original, there's further work done here to
	// parse more of the file and get magic comments. We skip this for
	// now until we know if it's useful for CUE.

	return invalidate, pkgFileChanged, importDeleted, pkgName
}

// validImports extracts the set of valid import paths from imports.
func validImports(imports []*cueast.ImportSpec) map[string]struct{} {
	pkgPaths := make(map[string]struct{})
	for _, imp := range imports {
		pkgPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		// Canonicalize the path.
		pkgPath = cueast.ParseImportPath(pkgPath).Canonical().String()
		pkgPaths[pkgPath] = struct{}{}
	}
	return pkgPaths
}
