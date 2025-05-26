package modregistry

import (
	"context"
	"io"
	"sort"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ordered parallel worker
// task submit have a comparable task id for sorting response
type parallelWorker[T any] struct {
	ctx    context.Context
	cancel context.CancelFunc

	wg     *sync.WaitGroup
	respCh chan *taskResponse[T]
	errCh  chan error
}

type taskResponse[T any] struct {
	taskId   int
	response T
}

func newParallelWorker[T any](ctx context.Context) *parallelWorker[T] {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &parallelWorker[T]{
		ctx:    cancelCtx,
		cancel: cancel,
		wg:     &sync.WaitGroup{},
		respCh: make(chan *taskResponse[T]),
		errCh:  make(chan error),
	}
}

func (w *parallelWorker[T]) run(taskId int, task func(context.Context) (T, error)) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		resp, err := task(w.ctx)
		if err != nil {
			w.errCh <- err
			return
		}
		w.respCh <- &taskResponse[T]{
			taskId:   taskId,
			response: resp,
		}
	}()
}

func (w *parallelWorker[T]) close() {
	close(w.errCh)
	close(w.respCh)
}

func (w *parallelWorker[T]) wait() ([]T, error) {
	defer w.close()

	done := make(chan struct{}, 1)
	go func() {
		w.wg.Wait()
		done <- struct{}{}
	}()

	var err error
	taskResps := []*taskResponse[T]{}
	for {
		select {
		case <-done:
			// sorting resp based on task id
			sort.Slice(taskResps, func(i, j int) bool {
				return taskResps[i].taskId < taskResps[j].taskId
			})

			// convert task response into raw response
			resp := make([]T, len(taskResps))
			for idx, r := range taskResps {
				resp[idx] = r.response
			}
			return resp, err
		case err = <-w.errCh:
			w.cancel()
			done <- struct{}{}
		case data := <-w.respCh:
			taskResps = append(taskResps, data)
		}
	}
}

type parallelBlobPusher struct {
	*parallelWorker[ocispec.Descriptor]
	loc RegistryLocation
}

func newParallelBlobPusher(ctx context.Context, loc RegistryLocation) *parallelBlobPusher {
	return &parallelBlobPusher{
		parallelWorker: newParallelWorker[ocispec.Descriptor](ctx),
		loc:            loc,
	}
}

func (w *parallelBlobPusher) run(taskId int, request *pushBlobRequest) {
	w.parallelWorker.run(taskId, func(ctx context.Context) (ocispec.Descriptor, error) {
		return w.loc.Registry.PushBlob(ctx, w.loc.Repository, request.desc, request.r)
	})
}

type pushBlobRequest struct {
	desc ocispec.Descriptor
	r    io.Reader
}

func parallelPushBlob(ctx context.Context, loc RegistryLocation, requests []*pushBlobRequest) error {
	w := newParallelBlobPusher(ctx, loc)
	for idx, r := range requests {
		w.run(idx, r)
	}
	_, err := w.wait()
	return err
}

func sliceMap[T any, V any](arr []T, fn func(T) V) []V {
	resp := make([]V, len(arr))
	for idx, item := range arr {
		resp[idx] = fn(item)
	}
	return resp
}
