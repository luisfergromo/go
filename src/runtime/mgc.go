// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO(rsc): The code having to do with the heap bitmap needs very serious cleanup.
// It has gotten completely out of control.

// Garbage collector (GC).
//
// The GC runs concurrently with mutator threads, is type accurate (aka precise), allows multiple
// GC thread to run in parallel. It is a concurrent mark and sweep that uses a write barrier. It is
// non-generational and non-compacting. Allocation is done using size segregated per P allocation
// areas to minimize fragmentation while eliminating locks in the common case.
//
// The algorithm decomposes into several steps.
// This is a high level description of the algorithm being used. For an overview of GC a good
// place to start is Richard Jones' gchandbook.org.
//
// The algorithm's intellectual heritage includes Dijkstra's on-the-fly algorithm, see
// Edsger W. Dijkstra, Leslie Lamport, A. J. Martin, C. S. Scholten, and E. F. M. Steffens. 1978.
// On-the-fly garbage collection: an exercise in cooperation. Commun. ACM 21, 11 (November 1978),
// 966-975.
// For journal quality proofs that these steps are complete, correct, and terminate see
// Hudson, R., and Moss, J.E.B. Copying Garbage Collection without stopping the world.
// Concurrency and Computation: Practice and Experience 15(3-5), 2003.
//
//  0. Set phase = GCscan from GCoff.
//  1. Wait for all P's to acknowledge phase change.
//         At this point all goroutines have passed through a GC safepoint and
//         know we are in the GCscan phase.
//  2. GC scans all goroutine stacks, mark and enqueues all encountered pointers
//       (marking avoids most duplicate enqueuing but races may produce benign duplication).
//       Preempted goroutines are scanned before P schedules next goroutine.
//  3. Set phase = GCmark.
//  4. Wait for all P's to acknowledge phase change.
//  5. Now write barrier marks and enqueues black, grey, or white to white pointers.
//       Malloc still allocates white (non-marked) objects.
//  6. Meanwhile GC transitively walks the heap marking reachable objects.
//  7. When GC finishes marking heap, it preempts P's one-by-one and
//       retakes partial wbufs (filled by write barrier or during a stack scan of the goroutine
//       currently scheduled on the P).
//  8. Once the GC has exhausted all available marking work it sets phase = marktermination.
//  9. Wait for all P's to acknowledge phase change.
// 10. Malloc now allocates black objects, so number of unmarked reachable objects
//        monotonically decreases.
// 11. GC preempts P's one-by-one taking partial wbufs and marks all unmarked yet
//        reachable objects.
// 12. When GC completes a full cycle over P's and discovers no new grey
//         objects, (which means all reachable objects are marked) set phase = GCsweep.
// 13. Wait for all P's to acknowledge phase change.
// 14. Now malloc allocates white (but sweeps spans before use).
//         Write barrier becomes nop.
// 15. GC does background sweeping, see description below.
// 16. When sweeping is complete set phase to GCoff.
// 17. When sufficient allocation has taken place replay the sequence starting at 0 above,
//         see discussion of GC rate below.

// Changing phases.
// Phases are changed by setting the gcphase to the next phase and possibly calling ackgcphase.
// All phase action must be benign in the presence of a change.
// Starting with GCoff
// GCoff to GCscan
//     GSscan scans stacks and globals greying them and never marks an object black.
//     Once all the P's are aware of the new phase they will scan gs on preemption.
//     This means that the scanning of preempted gs can't start until all the Ps
//     have acknowledged.
// GCscan to GCmark
//     GCMark turns on the write barrier which also only greys objects. No scanning
//     of objects (making them black) can happen until all the Ps have acknowledged
//     the phase change.
// GCmark to GCmarktermination
//     The only change here is that we start allocating black so the Ps must acknowledge
//     the change before we begin the termination algorithm
// GCmarktermination to GSsweep
//     Object currently on the freelist must be marked black for this to work.
//     Are things on the free lists black or white? How does the sweep phase work?

// Concurrent sweep.
//
// The sweep phase proceeds concurrently with normal program execution.
// The heap is swept span-by-span both lazily (when a goroutine needs another span)
// and concurrently in a background goroutine (this helps programs that are not CPU bound).
// At the end of STW mark termination all spans are marked as "needs sweeping".
//
// The background sweeper goroutine simply sweeps spans one-by-one.
//
// To avoid requesting more OS memory while there are unswept spans, when a
// goroutine needs another span, it first attempts to reclaim that much memory
// by sweeping. When a goroutine needs to allocate a new small-object span, it
// sweeps small-object spans for the same object size until it frees at least
// one object. When a goroutine needs to allocate large-object span from heap,
// it sweeps spans until it frees at least that many pages into heap. There is
// one case where this may not suffice: if a goroutine sweeps and frees two
// nonadjacent one-page spans to the heap, it will allocate a new two-page
// span, but there can still be other one-page unswept spans which could be
// combined into a two-page span.
//
// It's critical to ensure that no operations proceed on unswept spans (that would corrupt
// mark bits in GC bitmap). During GC all mcaches are flushed into the central cache,
// so they are empty. When a goroutine grabs a new span into mcache, it sweeps it.
// When a goroutine explicitly frees an object or sets a finalizer, it ensures that
// the span is swept (either by sweeping it, or by waiting for the concurrent sweep to finish).
// The finalizer goroutine is kicked off only when all spans are swept.
// When the next GC starts, it sweeps all not-yet-swept spans (if any).

// GC rate.
// Next GC is after we've allocated an extra amount of memory proportional to
// the amount already in use. The proportion is controlled by GOGC environment variable
// (100 by default). If GOGC=100 and we're using 4M, we'll GC again when we get to 8M
// (this mark is tracked in next_gc variable). This keeps the GC cost in linear
// proportion to the allocation cost. Adjusting GOGC just changes the linear constant
// (and also the amount of extra memory used).

package runtime

import "unsafe"

const (
	_DebugGC         = 0
	_ConcurrentSweep = true
	_FinBlockSize    = 4 * 1024
	_RootData        = 0
	_RootBss         = 1
	_RootFinalizers  = 2
	_RootSpans       = 3
	_RootFlushCaches = 4
	_RootCount       = 5
)

// heapminimum is the minimum number of bytes in the heap.
// This cleans up the corner case of where we have a very small live set but a lot
// of allocations and collecting every GOGC * live set is expensive.
var heapminimum = uint64(4 << 20)

// Initialized from $GOGC.  GOGC=off means no GC.
var gcpercent int32

func gcinit() {
	if unsafe.Sizeof(workbuf{}) != _WorkbufSize {
		throw("size of Workbuf is suboptimal")
	}

	work.markfor = parforalloc(_MaxGcproc)
	gcpercent = readgogc()
	for datap := &firstmoduledata; datap != nil; datap = datap.next {
		datap.gcdatamask = unrollglobgcprog((*byte)(unsafe.Pointer(datap.gcdata)), datap.edata-datap.data)
		datap.gcbssmask = unrollglobgcprog((*byte)(unsafe.Pointer(datap.gcbss)), datap.ebss-datap.bss)
	}
	memstats.next_gc = heapminimum
}

// gcenable is called after the bulk of the runtime initialization,
// just before we're about to start letting user code run.
// It kicks off the background sweeper goroutine and enables GC.
func gcenable() {
	c := make(chan int, 1)
	go bgsweep(c)
	<-c
	memstats.enablegc = true // now that runtime is initialized, GC is okay
}

func setGCPercent(in int32) (out int32) {
	lock(&mheap_.lock)
	out = gcpercent
	if in < 0 {
		in = -1
	}
	gcpercent = in
	unlock(&mheap_.lock)
	return out
}

// gcMarkWorkerMode represents the mode that a concurrent mark worker
// should operate in.
//
// Concurrent marking happens through four different mechanisms. One
// is mutator assists, which happen in response to allocations and are
// not scheduled. The other three are variations in the per-P mark
// workers and are distinguished by gcMarkWorkerMode.
type gcMarkWorkerMode int

const (
	// gcMarkWorkerDedicatedMode indicates that the P of a mark
	// worker is dedicated to running that mark worker. The mark
	// worker should run without preemption until concurrent mark
	// is done.
	gcMarkWorkerDedicatedMode gcMarkWorkerMode = iota

	// gcMarkWorkerFractionalMode indicates that a P is currently
	// running the "fractional" mark worker. The fractional worker
	// is necessary when GOMAXPROCS*gcGoalUtilization is not an
	// integer. The fractional worker should run until it is
	// preempted and will be scheduled to pick up the fractional
	// part of GOMAXPROCS*gcGoalUtilization.
	gcMarkWorkerFractionalMode

	// gcMarkWorkerIdleMode indicates that a P is running the mark
	// worker because it has nothing else to do. The idle worker
	// should run until it is preempted and account its time
	// against gcController.idleMarkTime.
	gcMarkWorkerIdleMode
)

// gcController implements the GC pacing controller that determines
// when to trigger concurrent garbage collection and how much marking
// work to do in mutator assists and background marking.
//
// It uses a feedback control algorithm to adjust the memstats.next_gc
// trigger based on the heap growth and GC CPU utilization each cycle.
// This algorithm optimizes for heap growth to match GOGC and for CPU
// utilization between assist and background marking to be 25% of
// GOMAXPROCS. The high-level design of this algorithm is documented
// at http://golang.org/s/go15gcpacing.
var gcController = gcControllerState{
	// Initial work ratio guess.
	//
	// TODO(austin): This is based on the work ratio of the
	// compiler on ./all.bash. Run a wider variety of programs and
	// see what their work ratios are.
	workRatioAvg: 0.5 / float64(ptrSize),

	// Initial trigger ratio guess.
	triggerRatio: 7 / 8.0,
}

type gcControllerState struct {
	// scanWork is the total scan work performed this cycle. This
	// is updated atomically during the cycle. Updates may be
	// batched arbitrarily, since the value is only read at the
	// end of the cycle.
	scanWork int64

	// bgScanCredit is the scan work credit accumulated by the
	// concurrent background scan. This credit is accumulated by
	// the background scan and stolen by mutator assists. This is
	// updated atomically. Updates occur in bounded batches, since
	// it is both written and read throughout the cycle.
	bgScanCredit int64

	// assistTime is the nanoseconds spent in mutator assists
	// during this cycle. This is updated atomically. Updates
	// occur in bounded batches, since it is both written and read
	// throughout the cycle.
	assistTime int64

	// dedicatedMarkTime is the nanoseconds spent in dedicated
	// mark workers during this cycle. This is updated atomically
	// at the end of the concurrent mark phase.
	dedicatedMarkTime int64

	// fractionalMarkTime is the nanoseconds spent in the
	// fractional mark worker during this cycle. This is updated
	// atomically throughout the cycle and will be up-to-date if
	// the fractional mark worker is not currently running.
	fractionalMarkTime int64

	// idleMarkTime is the nanoseconds spent in idle marking
	// during this cycle. This is udpated atomically throughout
	// the cycle.
	idleMarkTime int64

	// bgMarkStartTime is the absolute start time in nanoseconds
	// that the background mark phase started.
	bgMarkStartTime int64

	// initialHeapLive is the value of memstats.heap_live at the
	// beginning of this cycle.
	initialHeapLive uint64

	// heapGoal is the goal memstats.heap_live for when this cycle
	// ends. This is computed at the beginning of each cycle.
	heapGoal uint64

	// dedicatedMarkWorkersNeeded is the number of dedicated mark
	// workers that need to be started. This is computed at the
	// beginning of each cycle and decremented atomically as
	// dedicated mark workers get started.
	dedicatedMarkWorkersNeeded int64

	// workRatioAvg is a moving average of the scan work ratio
	// (scan work per byte marked).
	workRatioAvg float64

	// assistRatio is the ratio of allocated bytes to scan work
	// that should be performed by mutator assists. This is
	// computed at the beginning of each cycle.
	assistRatio float64

	// fractionalUtilizationGoal is the fraction of wall clock
	// time that should be spent in the fractional mark worker.
	// For example, if the overall mark utilization goal is 25%
	// and GOMAXPROCS is 6, one P will be a dedicated mark worker
	// and this will be set to 0.5 so that 50% of the time some P
	// is in a fractional mark worker. This is computed at the
	// beginning of each cycle.
	fractionalUtilizationGoal float64

	// triggerRatio is the heap growth ratio at which the garbage
	// collection cycle should start. E.g., if this is 0.6, then
	// GC should start when the live heap has reached 1.6 times
	// the heap size marked by the previous cycle. This is updated
	// at the end of of each cycle.
	triggerRatio float64

	_ [_CacheLineSize]byte

	// fractionalMarkWorkersNeeded is the number of fractional
	// mark workers that need to be started. This is either 0 or
	// 1. This is potentially updated atomically at every
	// scheduling point (hence it gets its own cache line).
	fractionalMarkWorkersNeeded int64

	_ [_CacheLineSize]byte
}

// startCycle resets the GC controller's state and computes estimates
// for a new GC cycle. The caller must hold worldsema.
func (c *gcControllerState) startCycle() {
	c.scanWork = 0
	c.bgScanCredit = 0
	c.assistTime = 0
	c.dedicatedMarkTime = 0
	c.fractionalMarkTime = 0
	c.idleMarkTime = 0
	c.initialHeapLive = memstats.heap_live

	// If this is the first GC cycle or we're operating on a very
	// small heap, fake heap_marked so it looks like next_gc is
	// the appropriate growth from heap_marked, even though the
	// real heap_marked may not have a meaningful value (on the
	// first cycle) or may be much smaller (resulting in a large
	// error response).
	if memstats.next_gc <= heapminimum {
		memstats.heap_marked = uint64(float64(memstats.next_gc) / (1 + c.triggerRatio))
	}

	// Compute the heap goal for this cycle
	c.heapGoal = memstats.heap_marked + memstats.heap_marked*uint64(gcpercent)/100

	// Compute the total mark utilization goal and divide it among
	// dedicated and fractional workers.
	totalUtilizationGoal := float64(gomaxprocs) * gcGoalUtilization
	c.dedicatedMarkWorkersNeeded = int64(totalUtilizationGoal)
	c.fractionalUtilizationGoal = totalUtilizationGoal - float64(c.dedicatedMarkWorkersNeeded)
	if c.fractionalUtilizationGoal > 0 {
		c.fractionalMarkWorkersNeeded = 1
	} else {
		c.fractionalMarkWorkersNeeded = 0
	}

	// Compute initial values for controls that are updated
	// throughout the cycle.
	c.revise()

	// Clear per-P state
	for _, p := range &allp {
		if p == nil {
			break
		}
		p.gcAssistTime = 0
	}

	return
}

// revise updates the assist ratio during the GC cycle to account for
// improved estimates. This should be called periodically during
// concurrent mark.
func (c *gcControllerState) revise() {
	// Estimate the size of the marked heap. We don't have much to
	// go on, so at the beginning of the cycle this uses the
	// marked heap size from last cycle. If the reachable heap has
	// grown since last cycle, we'll eventually mark more than
	// this and we can revise our estimate. This way, if we
	// overshoot our initial estimate, the assist ratio will climb
	// smoothly and put more pressure on mutator assists to finish
	// the cycle.
	heapMarkedEstimate := memstats.heap_marked
	if heapMarkedEstimate < work.bytesMarked {
		heapMarkedEstimate = work.bytesMarked
	}

	// Compute the expected work based on this estimate.
	scanWorkExpected := uint64(float64(heapMarkedEstimate) * c.workRatioAvg)

	// Compute the mutator assist ratio so by the time the mutator
	// allocates the remaining heap bytes up to next_gc, it will
	// have done (or stolen) the estimated amount of scan work.
	heapDistance := int64(c.heapGoal) - int64(c.initialHeapLive)
	if heapDistance <= 1024*1024 {
		// heapDistance can be negative if GC start is delayed
		// or if the allocation that pushed heap_live over
		// next_gc is large or if the trigger is really close
		// to GOGC. We don't want to set the assist negative
		// (or divide by zero, or set it really high), so
		// enforce a minimum on the distance.
		heapDistance = 1024 * 1024
	}
	c.assistRatio = float64(scanWorkExpected) / float64(heapDistance)
}

// endCycle updates the GC controller state at the end of the
// concurrent part of the GC cycle.
func (c *gcControllerState) endCycle() {
	// Proportional response gain for the trigger controller. Must
	// be in [0, 1]. Lower values smooth out transient effects but
	// take longer to respond to phase changes. Higher values
	// react to phase changes quickly, but are more affected by
	// transient changes. Values near 1 may be unstable.
	const triggerGain = 0.5

	// EWMA weight given to this cycle's scan work ratio.
	const workRatioWeight = 0.75

	// Compute next cycle trigger ratio. First, this computes the
	// "error" for this cycle; that is, how far off the trigger
	// was from what it should have been, accounting for both heap
	// growth and GC CPU utilization. We computing the actual heap
	// growth during this cycle and scale that by how far off from
	// the goal CPU utilization we were (to estimate the heap
	// growth if we had the desired CPU utilization). The
	// difference between this estimate and the GOGC-based goal
	// heap growth is the error.
	goalGrowthRatio := float64(gcpercent) / 100
	actualGrowthRatio := float64(memstats.heap_live)/float64(memstats.heap_marked) - 1
	duration := nanotime() - c.bgMarkStartTime
	utilization := float64(c.assistTime+c.dedicatedMarkTime+c.fractionalMarkTime) / float64(duration*int64(gomaxprocs))
	triggerError := goalGrowthRatio - c.triggerRatio - utilization/gcGoalUtilization*(actualGrowthRatio-c.triggerRatio)

	// Finally, we adjust the trigger for next time by this error,
	// damped by the proportional gain.
	c.triggerRatio += triggerGain * triggerError
	if c.triggerRatio < 0 {
		// This can happen if the mutator is allocating very
		// quickly or the GC is scanning very slowly.
		c.triggerRatio = 0
	} else if c.triggerRatio > goalGrowthRatio*0.95 {
		// Ensure there's always a little margin so that the
		// mutator assist ratio isn't infinity.
		c.triggerRatio = goalGrowthRatio * 0.95
	}

	// Compute the scan work ratio for this cycle.
	workRatio := float64(c.scanWork) / float64(work.bytesMarked)

	// Update EWMA of recent scan work ratios.
	c.workRatioAvg = workRatioWeight*workRatio + (1-workRatioWeight)*c.workRatioAvg
}

// findRunnable returns the background mark worker for _p_ if it
// should be run. This must only be called when gcphase == _GCmark.
func (c *gcControllerState) findRunnable(_p_ *p) *g {
	if gcphase != _GCmark {
		throw("gcControllerState.findRunnable: not in mark phase")
	}
	if _p_.gcBgMarkWorker == nil {
		throw("gcControllerState.findRunnable: no background mark worker")
	}
	if work.bgMarkDone != 0 {
		// Background mark is done. Don't schedule background
		// mark worker any more. (This is not just an
		// optimization. Without this we can spin scheduling
		// the background worker and having it return
		// immediately with no work to do.)
		return nil
	}
	if work.full == 0 && work.partial == 0 {
		// No work to be done right now. This can happen at
		// the end of the mark phase when there are still
		// assists tapering off. Don't bother running
		// background mark because it'll just return and
		// bgMarkCount might hover above zero.
		return nil
	}

	decIfPositive := func(ptr *int64) bool {
		if *ptr > 0 {
			if xaddint64(ptr, -1) >= 0 {
				return true
			}
			// We lost a race
			xaddint64(ptr, +1)
		}
		return false
	}

	if decIfPositive(&c.dedicatedMarkWorkersNeeded) {
		// This P is now dedicated to marking until the end of
		// the concurrent mark phase.
		_p_.gcMarkWorkerMode = gcMarkWorkerDedicatedMode
		// TODO(austin): This P isn't going to run anything
		// else for a while, so kick everything out of its run
		// queue.
	} else if decIfPositive(&c.fractionalMarkWorkersNeeded) {
		// This P has picked the token for the fractional
		// worker. If this P were to run the worker for the
		// next time slice, then at the end of that time
		// slice, would it be under the utilization goal?
		//
		// TODO(austin): We could fast path this and basically
		// eliminate contention on c.bgMarkCount by
		// precomputing the minimum time at which it's worth
		// next scheduling the fractional worker. Then Ps
		// don't have to fight in the window where we've
		// passed that deadline and no one has started the
		// worker yet.
		//
		// TODO(austin): Shorter preemption interval for mark
		// worker to improve fairness and give this
		// finer-grained control over schedule?
		now := nanotime() - gcController.bgMarkStartTime
		then := now + forcePreemptNS
		timeUsedIfRun := c.fractionalMarkTime + forcePreemptNS
		if float64(timeUsedIfRun)/float64(then) > c.fractionalUtilizationGoal {
			// Nope, we'd overshoot the utilization goal
			xaddint64(&c.fractionalMarkWorkersNeeded, +1)
			return nil
		}
		_p_.gcMarkWorkerMode = gcMarkWorkerFractionalMode
	} else {
		// All workers that need to be running are running
		return nil
	}

	// Run the background mark worker
	gp := _p_.gcBgMarkWorker
	casgstatus(gp, _Gwaiting, _Grunnable)
	if trace.enabled {
		traceGoUnpark(gp, 0)
	}
	return gp
}

// gcGoalUtilization is the goal CPU utilization for background
// marking as a fraction of GOMAXPROCS.
const gcGoalUtilization = 0.25

// gcBgCreditSlack is the amount of scan work credit background
// scanning can accumulate locally before updating
// gcController.bgScanCredit. Lower values give mutator assists more
// accurate accounting of background scanning. Higher values reduce
// memory contention.
const gcBgCreditSlack = 2000

// gcAssistTimeSlack is the nanoseconds of mutator assist time that
// can accumulate on a P before updating gcController.assistTime.
const gcAssistTimeSlack = 5000

// Determine whether to initiate a GC.
// If the GC is already working no need to trigger another one.
// This should establish a feedback loop where if the GC does not
// have sufficient time to complete then more memory will be
// requested from the OS increasing heap size thus allow future
// GCs more time to complete.
// memstat.heap_live read has a benign race.
// A false negative simple does not start a GC, a false positive
// will start a GC needlessly. Neither have correctness issues.
func shouldtriggergc() bool {
	return memstats.heap_live >= memstats.next_gc && atomicloaduint(&bggc.working) == 0
}

var work struct {
	full    uint64                // lock-free list of full blocks workbuf
	empty   uint64                // lock-free list of empty blocks workbuf
	partial uint64                // lock-free list of partially filled blocks workbuf
	pad0    [_CacheLineSize]uint8 // prevents false-sharing between full/empty and nproc/nwait
	nproc   uint32
	tstart  int64
	nwait   uint32
	ndone   uint32
	alldone note
	markfor *parfor

	bgMarkReady note   // signal background mark worker has started
	bgMarkDone  uint32 // cas to 1 when at a background mark completion point
	bgMarkNote  note   // signal background mark completion

	// Copy of mheap.allspans for marker or sweeper.
	spans []*mspan

	// totaltime is the CPU nanoseconds spent in GC since the
	// program started if debug.gctrace > 0.
	totaltime int64

	// bytesMarked is the number of bytes marked this cycle. This
	// includes bytes blackened in scanned objects, noscan objects
	// that go straight to black, and permagrey objects scanned by
	// markroot during the concurrent scan phase. This is updated
	// atomically during the cycle. Updates may be batched
	// arbitrarily, since the value is only read at the end of the
	// cycle.
	//
	// Because of benign races during marking, this number may not
	// be the exact number of marked bytes, but it should be very
	// close.
	bytesMarked uint64
}

// GC runs a garbage collection.
func GC() {
	startGC(gcForceBlockMode)
}

const (
	gcBackgroundMode = iota // concurrent GC
	gcForceMode             // stop-the-world GC now
	gcForceBlockMode        // stop-the-world GC now and wait for sweep
)

func startGC(mode int) {
	// The gc is turned off (via enablegc) until the bootstrap has completed.
	// Also, malloc gets called in the guts of a number of libraries that might be
	// holding locks. To avoid deadlocks during stoptheworld, don't bother
	// trying to run gc while holding a lock. The next mallocgc without a lock
	// will do the gc instead.
	mp := acquirem()
	if gp := getg(); gp == mp.g0 || mp.locks > 1 || !memstats.enablegc || panicking != 0 || gcpercent < 0 {
		releasem(mp)
		return
	}
	releasem(mp)
	mp = nil

	if mode != gcBackgroundMode {
		// special synchronous cases
		gc(mode)
		return
	}

	// trigger concurrent GC
	lock(&bggc.lock)
	if !bggc.started {
		bggc.working = 1
		bggc.started = true
		// This puts the G on the end of the current run
		// queue, so it may take a while to actually start.
		// This is only a problem for the first GC cycle.
		go backgroundgc()
	} else if bggc.working == 0 {
		bggc.working = 1
		if getg().m.lockedg != nil {
			// We can't directly switch to GC on a locked
			// M, so put it on the run queue and someone
			// will get to it.
			ready(bggc.g, 0)
		} else {
			unlock(&bggc.lock)
			readyExecute(bggc.g, 0)
			return
		}
	}
	unlock(&bggc.lock)
}

// State of the background concurrent GC goroutine.
var bggc struct {
	lock    mutex
	g       *g
	working uint
	started bool
}

// backgroundgc is running in a goroutine and does the concurrent GC work.
// bggc holds the state of the backgroundgc.
func backgroundgc() {
	bggc.g = getg()
	for {
		gc(gcBackgroundMode)
		lock(&bggc.lock)
		bggc.working = 0
		goparkunlock(&bggc.lock, "Concurrent GC wait", traceEvGoBlock, 1)
	}
}

func gc(mode int) {
	// debug.gctrace variables
	var stwprocs, maxprocs int32
	var tSweepTerm, tScan, tInstallWB, tMark, tMarkTerm int64
	var heap0, heap1, heap2 uint64

	// Ok, we're doing it!  Stop everybody else
	semacquire(&worldsema, false)

	// Pick up the remaining unswept/not being swept spans concurrently
	//
	// This shouldn't happen if we're being invoked in background
	// mode since proportional sweep should have just finished
	// sweeping everything, but rounding errors, etc, may leave a
	// few spans unswept. In forced mode, this is necessary since
	// GC can be forced at any point in the sweeping cycle.
	for gosweepone() != ^uintptr(0) {
		sweep.nbgsweep++
	}

	gctimer.count++
	if mode == gcBackgroundMode {
		gcBgMarkStartWorkers()
		gctimer.cycle.sweepterm = nanotime()
	}
	if debug.gctrace > 0 {
		stwprocs, maxprocs = gcprocs(), gomaxprocs
		tSweepTerm = nanotime()
		if mode == gcBackgroundMode {
			// We started GC when heap_live == next_gc,
			// but the mutator may have allocated between
			// then and now. Report heap when GC started.
			heap0 = memstats.next_gc
		} else {
			heap0 = memstats.heap_live
		}
	}

	if trace.enabled {
		traceGCStart()
	}

	systemstack(stoptheworld)
	systemstack(finishsweep_m) // finish sweep before we start concurrent scan.
	// clearpools before we start the GC. If we wait they memory will not be
	// reclaimed until the next GC cycle.
	clearpools()

	work.bytesMarked = 0

	if mode == gcBackgroundMode { // Do as much work concurrently as possible
		gcController.startCycle()

		systemstack(func() {
			gcphase = _GCscan

			// Concurrent scan.
			starttheworld()
			gctimer.cycle.scan = nanotime()
			if debug.gctrace > 0 {
				tScan = nanotime()
			}
			gcscan_m()
			gctimer.cycle.installmarkwb = nanotime()

			// Enter mark phase, enabling write barriers
			// and mutator assists.
			//
			// TODO: Elimate this STW. This requires
			// enabling write barriers in all mutators
			// before enabling any mutator assists or
			// background marking.
			if debug.gctrace > 0 {
				tInstallWB = nanotime()
			}
			stoptheworld()
			gcBgMarkPrepare()
			gcphase = _GCmark

			// Concurrent mark.
			starttheworld()
		})
		gctimer.cycle.mark = nanotime()
		if debug.gctrace > 0 {
			tMark = nanotime()
		}
		for !notetsleepg(&work.bgMarkNote, 10*1000*1000) {
			gcController.revise()
		}
		noteclear(&work.bgMarkNote)

		// Begin mark termination.
		gctimer.cycle.markterm = nanotime()
		if debug.gctrace > 0 {
			tMarkTerm = nanotime()
		}
		systemstack(stoptheworld)
		// The gcphase is _GCmark, it will transition to _GCmarktermination
		// below. The important thing is that the wb remains active until
		// all marking is complete. This includes writes made by the GC.

		gcController.endCycle()
	} else {
		// For non-concurrent GC (mode != gcBackgroundMode)
		// The g stacks have not been scanned so clear g state
		// such that mark termination scans all stacks.
		gcResetGState()

		if debug.gctrace > 0 {
			t := nanotime()
			tScan, tInstallWB, tMark, tMarkTerm = t, t, t, t
		}
	}

	// World is stopped.
	// Start marktermination which includes enabling the write barrier.
	gcphase = _GCmarktermination

	if debug.gctrace > 0 {
		heap1 = memstats.heap_live
	}

	startTime := nanotime()

	mp := acquirem()
	mp.preemptoff = "gcing"
	_g_ := getg()
	_g_.m.traceback = 2
	gp := _g_.m.curg
	casgstatus(gp, _Grunning, _Gwaiting)
	gp.waitreason = "garbage collection"

	// Run gc on the g0 stack.  We do this so that the g stack
	// we're currently running on will no longer change.  Cuts
	// the root set down a bit (g0 stacks are not scanned, and
	// we don't need to scan gc's internal state).  We also
	// need to switch to g0 so we can shrink the stack.
	systemstack(func() {
		gcMark(startTime)
		if debug.gctrace > 0 {
			heap2 = work.bytesMarked
		}
		if debug.gccheckmark > 0 {
			// Run a full stop-the-world mark using checkmark bits,
			// to check that we didn't forget to mark anything during
			// the concurrent mark process.
			initCheckmarks()
			gcMark(startTime)
			clearCheckmarks()
		}

		// marking is complete so we can turn the write barrier off
		gcphase = _GCoff
		gcSweep(mode)

		if debug.gctrace > 1 {
			startTime = nanotime()
			// The g stacks have been scanned so
			// they have gcscanvalid==true and gcworkdone==true.
			// Reset these so that all stacks will be rescanned.
			gcResetGState()
			finishsweep_m()

			// Still in STW but gcphase is _GCoff, reset to _GCmarktermination
			// At this point all objects will be found during the gcMark which
			// does a complete STW mark and object scan.
			gcphase = _GCmarktermination
			gcMark(startTime)
			gcphase = _GCoff // marking is done, turn off wb.
			gcSweep(mode)
		}
	})

	_g_.m.traceback = 0
	casgstatus(gp, _Gwaiting, _Grunning)

	if trace.enabled {
		traceGCDone()
	}

	// all done
	mp.preemptoff = ""

	if mode == gcBackgroundMode {
		gctimer.cycle.sweep = nanotime()
	}

	semrelease(&worldsema)

	if mode == gcBackgroundMode {
		if gctimer.verbose > 1 {
			GCprinttimes()
		} else if gctimer.verbose > 0 {
			calctimes() // ignore result
		}
	}

	if gcphase != _GCoff {
		throw("gc done but gcphase != _GCoff")
	}

	systemstack(starttheworld)

	releasem(mp)
	mp = nil

	memstats.numgc++
	if debug.gctrace > 0 {
		tEnd := nanotime()

		// Update work.totaltime
		sweepTermCpu := int64(stwprocs) * (tScan - tSweepTerm)
		scanCpu := tInstallWB - tScan
		installWBCpu := int64(stwprocs) * (tMark - tInstallWB)
		// We report idle marking time below, but omit it from
		// the overall utilization here since it's "free".
		markCpu := gcController.assistTime + gcController.dedicatedMarkTime + gcController.fractionalMarkTime
		markTermCpu := int64(stwprocs) * (tEnd - tMarkTerm)
		cycleCpu := sweepTermCpu + scanCpu + installWBCpu + markCpu + markTermCpu
		work.totaltime += cycleCpu

		// Compute overall utilization
		totalCpu := sched.totaltime + (tEnd-sched.procresizetime)*int64(gomaxprocs)
		util := work.totaltime * 100 / totalCpu

		var sbuf [24]byte
		printlock()
		print("gc #", memstats.numgc,
			" @", string(itoaDiv(sbuf[:], uint64(tEnd-runtimeInitTime)/1e6, 3)), "s ",
			util, "%: ",
			(tScan-tSweepTerm)/1e6,
			"+", (tInstallWB-tScan)/1e6,
			"+", (tMark-tInstallWB)/1e6,
			"+", (tMarkTerm-tMark)/1e6,
			"+", (tEnd-tMarkTerm)/1e6, " ms clock, ",
			sweepTermCpu/1e6,
			"+", scanCpu/1e6,
			"+", installWBCpu/1e6,
			"+", gcController.assistTime/1e6,
			"/", (gcController.dedicatedMarkTime+gcController.fractionalMarkTime)/1e6,
			"/", gcController.idleMarkTime/1e6,
			"+", markTermCpu/1e6, " ms cpu, ",
			heap0>>20, "->", heap1>>20, "->", heap2>>20, " MB, ",
			maxprocs, " P")
		if mode != gcBackgroundMode {
			print(" (forced)")
		}
		print("\n")
		printunlock()
	}
	sweep.nbgsweep = 0
	sweep.npausesweep = 0

	// now that gc is done, kick off finalizer thread if needed
	if !concurrentSweep {
		// give the queued finalizers, if any, a chance to run
		Gosched()
	}
}

// gcBgMarkStartWorkers prepares background mark worker goroutines.
// These goroutines will not run until the mark phase, but they must
// be started while the work is not stopped and from a regular G
// stack. The caller must hold worldsema.
func gcBgMarkStartWorkers() {
	// Background marking is performed by per-P G's. Ensure that
	// each P has a background GC G.
	for _, p := range &allp {
		if p == nil || p.status == _Pdead {
			break
		}
		if p.gcBgMarkWorker == nil {
			go gcBgMarkWorker(p)
			notetsleepg(&work.bgMarkReady, -1)
			noteclear(&work.bgMarkReady)
		}
	}
}

// gcBgMarkPrepare sets up state for background marking.
// Mutator assists must not yet be enabled.
func gcBgMarkPrepare() {
	// Background marking will stop when the work queues are empty
	// and there are no more workers (note that, since this is
	// concurrent, this may be a transient state, but mark
	// termination will clean it up). Between background workers
	// and assists, we don't really know how many workers there
	// will be, so we pretend to have an arbitrarily large number
	// of workers, almost all of which are "waiting". While a
	// worker is working it decrements nwait. If nproc == nwait,
	// there are no workers.
	work.nproc = ^uint32(0)
	work.nwait = ^uint32(0)

	// Background GC and assists race to set this to 1 on
	// completion so that this only gets one "done" signal.
	work.bgMarkDone = 0

	gcController.bgMarkStartTime = nanotime()
}

func gcBgMarkWorker(p *p) {
	// Register this G as the background mark worker for p.
	if p.gcBgMarkWorker != nil {
		throw("P already has a background mark worker")
	}
	gp := getg()

	mp := acquirem()
	p.gcBgMarkWorker = gp
	// After this point, the background mark worker is scheduled
	// cooperatively by gcController.findRunnable. Hence, it must
	// never be preempted, as this would put it into _Grunnable
	// and put it on a run queue. Instead, when the preempt flag
	// is set, this puts itself into _Gwaiting to be woken up by
	// gcController.findRunnable at the appropriate time.
	notewakeup(&work.bgMarkReady)
	var gcw gcWork
	for {
		// Go to sleep until woken by gcContoller.findRunnable.
		// We can't releasem yet since even the call to gopark
		// may be preempted.
		gopark(func(g *g, mp unsafe.Pointer) bool {
			releasem((*m)(mp))
			return true
		}, unsafe.Pointer(mp), "mark worker (idle)", traceEvGoBlock, 0)

		// Loop until the P dies and disassociates this
		// worker. (The P may later be reused, in which case
		// it will get a new worker.)
		if p.gcBgMarkWorker != gp {
			break
		}

		// Disable preemption so we can use the gcw. If the
		// scheduler wants to preempt us, we'll stop draining,
		// dispose the gcw, and then preempt.
		mp = acquirem()

		startTime := nanotime()

		xadd(&work.nwait, -1)

		done := false
		switch p.gcMarkWorkerMode {
		case gcMarkWorkerDedicatedMode:
			gcDrain(&gcw, gcBgCreditSlack)
			// gcDrain did the xadd(&work.nwait +1) to
			// match the decrement above. It only returns
			// at a mark completion point.
			done = true
		case gcMarkWorkerFractionalMode, gcMarkWorkerIdleMode:
			gcDrainUntilPreempt(&gcw, gcBgCreditSlack)
			// Was this the last worker and did we run out
			// of work?
			done = xadd(&work.nwait, +1) == work.nproc && work.full == 0 && work.partial == 0
		}
		gcw.dispose()

		// If this is the first worker to reach a background
		// completion point this cycle, signal the coordinator.
		if done && cas(&work.bgMarkDone, 0, 1) {
			notewakeup(&work.bgMarkNote)
		}

		duration := nanotime() - startTime
		switch p.gcMarkWorkerMode {
		case gcMarkWorkerDedicatedMode:
			xaddint64(&gcController.dedicatedMarkTime, duration)
		case gcMarkWorkerFractionalMode:
			xaddint64(&gcController.fractionalMarkTime, duration)
			xaddint64(&gcController.fractionalMarkWorkersNeeded, 1)
		case gcMarkWorkerIdleMode:
			xaddint64(&gcController.idleMarkTime, duration)
		}
	}
}

// gcMark runs the mark (or, for concurrent GC, mark termination)
// STW is in effect at this point.
//TODO go:nowritebarrier
func gcMark(start_time int64) {
	if debug.allocfreetrace > 0 {
		tracegc()
	}

	if gcphase != _GCmarktermination {
		throw("in gcMark expecting to see gcphase as _GCmarktermination")
	}
	t0 := start_time
	work.tstart = start_time

	gcCopySpans() // TODO(rlh): should this be hoisted and done only once? Right now it is done for normal marking and also for checkmarking.

	work.nwait = 0
	work.ndone = 0
	work.nproc = uint32(gcprocs())

	if trace.enabled {
		traceGCScanStart()
	}

	parforsetup(work.markfor, work.nproc, uint32(_RootCount+allglen), false, markroot)
	if work.nproc > 1 {
		noteclear(&work.alldone)
		helpgc(int32(work.nproc))
	}

	harvestwbufs() // move local workbufs onto global queues where the GC can find them
	gchelperstart()
	parfordo(work.markfor)
	var gcw gcWork
	gcDrain(&gcw, -1)
	gcw.dispose()

	if work.full != 0 {
		throw("work.full != 0")
	}
	if work.partial != 0 {
		throw("work.partial != 0")
	}

	if work.nproc > 1 {
		notesleep(&work.alldone)
	}

	if trace.enabled {
		traceGCScanDone()
	}

	shrinkfinish()

	cachestats()

	// Trigger the next GC cycle when the allocated heap has
	// grown by triggerRatio over the marked heap size.
	memstats.heap_live = work.bytesMarked
	memstats.heap_marked = work.bytesMarked
	memstats.next_gc = uint64(float64(memstats.heap_live) * (1 + gcController.triggerRatio))
	if memstats.next_gc < heapminimum {
		memstats.next_gc = heapminimum
	}

	if trace.enabled {
		traceHeapAlloc()
		traceNextGC()
	}

	t4 := nanotime()
	atomicstore64(&memstats.last_gc, uint64(unixnanotime())) // must be Unix time to make sense to user
	memstats.pause_ns[memstats.numgc%uint32(len(memstats.pause_ns))] = uint64(t4 - t0)
	memstats.pause_end[memstats.numgc%uint32(len(memstats.pause_end))] = uint64(t4)
	memstats.pause_total_ns += uint64(t4 - t0)
}

func gcSweep(mode int) {
	if gcphase != _GCoff {
		throw("gcSweep being done but phase is not GCoff")
	}
	gcCopySpans()

	lock(&mheap_.lock)
	mheap_.sweepgen += 2
	mheap_.sweepdone = 0
	sweep.spanidx = 0
	unlock(&mheap_.lock)

	if !_ConcurrentSweep || mode == gcForceBlockMode {
		// Special case synchronous sweep.
		// Record that no proportional sweeping has to happen.
		lock(&mheap_.lock)
		mheap_.sweepPagesPerByte = 0
		mheap_.pagesSwept = 0
		unlock(&mheap_.lock)
		// Sweep all spans eagerly.
		for sweepone() != ^uintptr(0) {
			sweep.npausesweep++
		}
		// Do an additional mProf_GC, because all 'free' events are now real as well.
		mProf_GC()
		mProf_GC()
		return
	}

	// Account how much sweeping needs to be done before the next
	// GC cycle and set up proportional sweep statistics.
	var pagesToSweep uintptr
	for _, s := range work.spans {
		if s.state == mSpanInUse {
			pagesToSweep += s.npages
		}
	}
	heapDistance := int64(memstats.next_gc) - int64(memstats.heap_live)
	// Add a little margin so rounding errors and concurrent
	// sweep are less likely to leave pages unswept when GC starts.
	heapDistance -= 1024 * 1024
	if heapDistance < _PageSize {
		// Avoid setting the sweep ratio extremely high
		heapDistance = _PageSize
	}
	lock(&mheap_.lock)
	mheap_.sweepPagesPerByte = float64(pagesToSweep) / float64(heapDistance)
	mheap_.pagesSwept = 0
	unlock(&mheap_.lock)

	// Background sweep.
	lock(&sweep.lock)
	if sweep.parked {
		sweep.parked = false
		ready(sweep.g, 0)
	}
	unlock(&sweep.lock)
	mProf_GC()
}

func gcCopySpans() {
	// Cache runtime.mheap_.allspans in work.spans to avoid conflicts with
	// resizing/freeing allspans.
	// New spans can be created while GC progresses, but they are not garbage for
	// this round:
	//  - new stack spans can be created even while the world is stopped.
	//  - new malloc spans can be created during the concurrent sweep
	// Even if this is stop-the-world, a concurrent exitsyscall can allocate a stack from heap.
	lock(&mheap_.lock)
	// Free the old cached mark array if necessary.
	if work.spans != nil && &work.spans[0] != &h_allspans[0] {
		sysFree(unsafe.Pointer(&work.spans[0]), uintptr(len(work.spans))*unsafe.Sizeof(work.spans[0]), &memstats.other_sys)
	}
	// Cache the current array for sweeping.
	mheap_.gcspans = mheap_.allspans
	work.spans = h_allspans
	unlock(&mheap_.lock)
}

// gcResetGState resets the GC state of all G's and returns the length
// of allgs.
func gcResetGState() (numgs int) {
	// This may be called during a concurrent phase, so make sure
	// allgs doesn't change.
	lock(&allglock)
	for _, gp := range allgs {
		gp.gcworkdone = false  // set to true in gcphasework
		gp.gcscanvalid = false // stack has not been scanned
		gp.gcalloc = 0
		gp.gcscanwork = 0
	}
	numgs = len(allgs)
	unlock(&allglock)
	return
}

// Hooks for other packages

var poolcleanup func()

//go:linkname sync_runtime_registerPoolCleanup sync.runtime_registerPoolCleanup
func sync_runtime_registerPoolCleanup(f func()) {
	poolcleanup = f
}

func clearpools() {
	// clear sync.Pools
	if poolcleanup != nil {
		poolcleanup()
	}

	// Clear central sudog cache.
	// Leave per-P caches alone, they have strictly bounded size.
	// Disconnect cached list before dropping it on the floor,
	// so that a dangling ref to one entry does not pin all of them.
	lock(&sched.sudoglock)
	var sg, sgnext *sudog
	for sg = sched.sudogcache; sg != nil; sg = sgnext {
		sgnext = sg.next
		sg.next = nil
	}
	sched.sudogcache = nil
	unlock(&sched.sudoglock)

	// Clear central defer pools.
	// Leave per-P pools alone, they have strictly bounded size.
	lock(&sched.deferlock)
	for i := range sched.deferpool {
		// disconnect cached list before dropping it on the floor,
		// so that a dangling ref to one entry does not pin all of them.
		var d, dlink *_defer
		for d = sched.deferpool[i]; d != nil; d = dlink {
			dlink = d.link
			d.link = nil
		}
		sched.deferpool[i] = nil
	}
	unlock(&sched.deferlock)

	for _, p := range &allp {
		if p == nil {
			break
		}
		// clear tinyalloc pool
		if c := p.mcache; c != nil {
			c.tiny = nil
			c.tinyoffset = 0
		}
	}
}

// Timing

//go:nowritebarrier
func gchelper() {
	_g_ := getg()
	_g_.m.traceback = 2
	gchelperstart()

	if trace.enabled {
		traceGCScanStart()
	}

	// parallel mark for over GC roots
	parfordo(work.markfor)
	if gcphase != _GCscan {
		var gcw gcWork
		gcDrain(&gcw, -1) // blocks in getfull
		gcw.dispose()
	}

	if trace.enabled {
		traceGCScanDone()
	}

	nproc := work.nproc // work.nproc can change right after we increment work.ndone
	if xadd(&work.ndone, +1) == nproc-1 {
		notewakeup(&work.alldone)
	}
	_g_.m.traceback = 0
}

func gchelperstart() {
	_g_ := getg()

	if _g_.m.helpgc < 0 || _g_.m.helpgc >= _MaxGcproc {
		throw("gchelperstart: bad m->helpgc")
	}
	if _g_ != _g_.m.g0 {
		throw("gchelper not running on g0 stack")
	}
}

// gcchronograph holds timer information related to GC phases
// max records the maximum time spent in each GC phase since GCstarttimes.
// total records the total time spent in each GC phase since GCstarttimes.
// cycle records the absolute time (as returned by nanoseconds()) that each GC phase last started at.
type gcchronograph struct {
	count    int64
	verbose  int64
	maxpause int64
	max      gctimes
	total    gctimes
	cycle    gctimes
}

// gctimes records the time in nanoseconds of each phase of the concurrent GC.
type gctimes struct {
	sweepterm     int64 // stw
	scan          int64
	installmarkwb int64 // stw
	mark          int64
	markterm      int64 // stw
	sweep         int64
}

var gctimer gcchronograph

// GCstarttimes initializes the gc times. All previous times are lost.
func GCstarttimes(verbose int64) {
	gctimer = gcchronograph{verbose: verbose}
}

// GCendtimes stops the gc timers.
func GCendtimes() {
	gctimer.verbose = 0
}

// calctimes converts gctimer.cycle into the elapsed times, updates gctimer.total
// and updates gctimer.max with the max pause time.
func calctimes() gctimes {
	var times gctimes

	var max = func(a, b int64) int64 {
		if a > b {
			return a
		}
		return b
	}

	times.sweepterm = gctimer.cycle.scan - gctimer.cycle.sweepterm
	gctimer.total.sweepterm += times.sweepterm
	gctimer.max.sweepterm = max(gctimer.max.sweepterm, times.sweepterm)
	gctimer.maxpause = max(gctimer.maxpause, gctimer.max.sweepterm)

	times.scan = gctimer.cycle.installmarkwb - gctimer.cycle.scan
	gctimer.total.scan += times.scan
	gctimer.max.scan = max(gctimer.max.scan, times.scan)

	times.installmarkwb = gctimer.cycle.mark - gctimer.cycle.installmarkwb
	gctimer.total.installmarkwb += times.installmarkwb
	gctimer.max.installmarkwb = max(gctimer.max.installmarkwb, times.installmarkwb)
	gctimer.maxpause = max(gctimer.maxpause, gctimer.max.installmarkwb)

	times.mark = gctimer.cycle.markterm - gctimer.cycle.mark
	gctimer.total.mark += times.mark
	gctimer.max.mark = max(gctimer.max.mark, times.mark)

	times.markterm = gctimer.cycle.sweep - gctimer.cycle.markterm
	gctimer.total.markterm += times.markterm
	gctimer.max.markterm = max(gctimer.max.markterm, times.markterm)
	gctimer.maxpause = max(gctimer.maxpause, gctimer.max.markterm)

	return times
}

// GCprinttimes prints latency information in nanoseconds about various
// phases in the GC. The information for each phase includes the maximum pause
// and total time since the most recent call to GCstarttimes as well as
// the information from the most recent Concurent GC cycle. Calls from the
// application to runtime.GC() are ignored.
func GCprinttimes() {
	if gctimer.verbose == 0 {
		println("GC timers not enabled")
		return
	}

	// Explicitly put times on the heap so printPhase can use it.
	times := new(gctimes)
	*times = calctimes()
	cycletime := gctimer.cycle.sweep - gctimer.cycle.sweepterm
	pause := times.sweepterm + times.installmarkwb + times.markterm
	gomaxprocs := GOMAXPROCS(-1)

	printlock()
	print("GC: #", gctimer.count, " ", cycletime, "ns @", gctimer.cycle.sweepterm, " pause=", pause, " maxpause=", gctimer.maxpause, " goroutines=", allglen, " gomaxprocs=", gomaxprocs, "\n")
	printPhase := func(label string, get func(*gctimes) int64, procs int) {
		print("GC:     ", label, " ", get(times), "ns\tmax=", get(&gctimer.max), "\ttotal=", get(&gctimer.total), "\tprocs=", procs, "\n")
	}
	printPhase("sweep term:", func(t *gctimes) int64 { return t.sweepterm }, gomaxprocs)
	printPhase("scan:      ", func(t *gctimes) int64 { return t.scan }, 1)
	printPhase("install wb:", func(t *gctimes) int64 { return t.installmarkwb }, gomaxprocs)
	printPhase("mark:      ", func(t *gctimes) int64 { return t.mark }, 1)
	printPhase("mark term: ", func(t *gctimes) int64 { return t.markterm }, gomaxprocs)
	printunlock()
}

// itoaDiv formats val/(10**dec) into buf.
func itoaDiv(buf []byte, val uint64, dec int) []byte {
	i := len(buf) - 1
	idec := i - dec
	for val >= 10 || i >= idec {
		buf[i] = byte(val%10 + '0')
		i--
		if i == idec {
			buf[i] = '.'
			i--
		}
		val /= 10
	}
	buf[i] = byte(val + '0')
	return buf[i:]
}
