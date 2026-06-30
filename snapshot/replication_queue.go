package snapshot

import (
	"container/heap"
	"sync"
)

// replicationJob is one unit of post-capture replication work: push the
// snapshot for (CapsuleID, Tag) out to enough peers to reach the
// replication factor K.
type replicationJob struct {
	capsuleID string
	tag       string
	checksum  string
	size      int64

	// priority is "urgency": higher means fewer existing copies, so a
	// capsule at 0 replicated copies (priority == K) outranks one already
	// at K-1 copies (priority == 1). The worker pool always pulls the
	// highest-priority job first (thundering-herd control, plan part 8).
	priority int

	// seq is a monotonically increasing enqueue sequence used purely as a
	// FIFO tiebreak between equal-priority jobs so ordering is
	// deterministic.
	seq uint64
}

// jobHeap is a max-heap on priority, FIFO (min seq) tiebreak. It is the
// raw container/heap backing store; all access is serialized by jobQueue's
// mutex, so jobHeap itself needs no locking.
type jobHeap []replicationJob

func (h jobHeap) Len() int { return len(h) }
func (h jobHeap) Less(i, j int) bool {
	if h[i].priority != h[j].priority {
		return h[i].priority > h[j].priority // higher priority first
	}
	return h[i].seq < h[j].seq // older first
}
func (h jobHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *jobHeap) Push(x any) { *h = append(*h, x.(replicationJob)) }
func (h *jobHeap) Pop() any {
	old := *h
	n := len(old)
	job := old[n-1]
	*h = old[:n-1]
	return job
}

// jobQueue is a bounded, priority-ordered, blocking work queue feeding the
// replication worker pool. It bounds memory (rule: every queue has a cap)
// and, on overflow, drops the least-urgent job so that the most
// under-replicated capsules always make progress.
type jobQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	heap   jobHeap
	cap    int
	closed bool
}

// newJobQueue creates a bounded priority queue with the given capacity.
// A non-positive capacity is treated as 1 (a queue must hold at least one
// job).
func newJobQueue(capacity int) *jobQueue {
	if capacity < 1 {
		capacity = 1
	}
	q := &jobQueue{cap: capacity}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push enqueues a job. When the queue is at capacity it evicts the
// least-urgent job (lowest priority, then newest) if the incoming job is
// strictly more urgent; otherwise the incoming job is dropped. Returns
// true if the job was accepted, and the dropped job (if any) for logging.
func (q *jobQueue) push(job replicationJob) (accepted bool, dropped *replicationJob, droppedOK bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false, nil, false
	}

	if len(q.heap) < q.cap {
		heap.Push(&q.heap, job)
		q.cond.Signal()
		return true, nil, false
	}

	// At capacity: find the least-urgent resident job.
	worstIdx := 0
	for i := 1; i < len(q.heap); i++ {
		if q.heap[i].priority < q.heap[worstIdx].priority ||
			(q.heap[i].priority == q.heap[worstIdx].priority && q.heap[i].seq > q.heap[worstIdx].seq) {
			worstIdx = i
		}
	}
	worst := q.heap[worstIdx]
	// Accept the newcomer only if it is strictly more urgent than the
	// worst resident; ties keep the existing (FIFO) job.
	if job.priority <= worst.priority {
		return false, &job, true
	}
	heap.Remove(&q.heap, worstIdx)
	heap.Push(&q.heap, job)
	q.cond.Signal()
	return true, &worst, true
}

// pop blocks until a job is available or the queue is closed. It returns
// the highest-priority job. The second return value is false once the
// queue is closed and drained, signalling the worker to exit.
func (q *jobQueue) pop() (replicationJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.heap) == 0 {
		if q.closed {
			return replicationJob{}, false
		}
		q.cond.Wait()
	}
	job := heap.Pop(&q.heap).(replicationJob)
	return job, true
}

// close marks the queue closed and wakes every blocked worker so they can
// exit. Jobs still resident are discarded (Stop must be prompt, not
// draining — in-flight pushes are aborted via the Replicator context).
func (q *jobQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.heap = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}

// length returns the current number of queued jobs (test/observability).
func (q *jobQueue) length() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.heap)
}
