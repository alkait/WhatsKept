package postprocess

import (
	"context"
	"database/sql"
	"sync"
)

// runWorkerPipeline is the worker-pool + collector driving every
// enrichment phase (image download/describe, voice download/transcribe).
// The mechanics — a bounded worker pool, a feeder that stops on
// cancellation, first-FatalError-wins abort, throttled progress, and an
// optional FTS rebuild — are identical across phases; only the per-item
// work and how a result folds into the run tally differ, so those are
// supplied as callbacks.
//
// C is the candidate (job) type, R the per-item result. DB writes are
// serialised by capping the SQLite pool to a single connection.
//
// Callbacks:
//   - process:  does the per-item work (runs on a worker goroutine).
//   - statusOf: extracts a result's terminal status and global-abort error.
//     A non-nil error aborts the whole run; statusCancelled is skipped.
//   - onFatal:  called once, for the first fatal result (nil to ignore).
//   - tally:    folds one successful (non-cancelled, non-fatal) result into
//     the caller's accumulator, including any per-phase counters.
//   - onProgress: emits a progress update (already throttled to every
//     progressEvery results, plus once at the end). nil disables progress.
//   - onDone:   called once after draining with stoppedByUser =
//     (no fatal && caller ctx cancelled); records duration / cancelled flag.
//   - rebuildFTS: optional post-run index rebuild (nil to skip).
func runWorkerPipeline[C any, R any](
	ctx context.Context,
	db *sql.DB,
	candidates []C,
	concurrency, progressEvery int,
	process func(context.Context, C) R,
	statusOf func(R) (status string, fatal error),
	onFatal func(error),
	tally func(R),
	onProgress func(),
	onDone func(stoppedByUser bool),
	rebuildFTS func(),
) error {
	if progressEvery <= 0 {
		progressEvery = 1 // guard the progress-emit modulo
	}
	db.SetMaxOpenConns(1)

	// runCtx is cancelled either by the caller (user Stop) or internally
	// when a worker hits a FatalError (bad key / no credits) — both make
	// the feeder stop and the workers drain.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var fatalErr error
	var fatalMu sync.Mutex

	jobsCh := make(chan C)
	resCh := make(chan R, concurrency)

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobsCh {
				resCh <- process(runCtx, c)
			}
		}()
	}
	// Feeder: stops early (closing jobsCh) when the run context is
	// cancelled, so workers drain and exit.
	go func() {
		defer close(jobsCh)
		for _, c := range candidates {
			select {
			case <-runCtx.Done():
				return
			case jobsCh <- c:
			}
		}
	}()
	go func() { wg.Wait(); close(resCh) }()

	processed := 0
	for r := range resCh {
		status, fatal := statusOf(r)
		if fatal != nil {
			// First fatal wins; cancel the run so the feeder/workers
			// wind down without marking the rest as errored.
			fatalMu.Lock()
			if fatalErr == nil {
				fatalErr = fatal
				if onFatal != nil {
					onFatal(fatal)
				}
			}
			fatalMu.Unlock()
			cancelRun()
			continue
		}
		if status == statusCancelled {
			continue // in-flight item aborted by cancel; not a real outcome
		}
		tally(r)
		processed++
		if onProgress != nil && processed%progressEvery == 0 {
			onProgress()
		}
	}

	// A user Stop cancels ctx; a fatal abort cancels only runCtx.
	onDone(fatalErr == nil && ctx.Err() != nil)
	if onProgress != nil {
		onProgress()
	}
	if rebuildFTS != nil {
		rebuildFTS()
	}
	return fatalErr
}
