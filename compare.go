package main

const (
	// BATCH_FILES and BATCH_BYTES bound how many small files are hashed in one request.
	BATCH_FILES = 256
	BATCH_BYTES = 32 * 1024 * 1024
)

// makeBatches groups jobs for the workers. With batching, files up to PRECHECK_SIZE are grouped
// so that a remote node hashes many of them per request, while larger files run alone.
func makeBatches(jobs []CompareJob, sizes map[string]int64, batching bool) [][]CompareJob {
	var batches [][]CompareJob
	var current []CompareJob
	var currentBytes int64
	for _, j := range jobs {
		size := sizes[j.PathA]
		if !batching || size > PRECHECK_SIZE {
			batches = append(batches, []CompareJob{j})
			continue
		}
		if len(current) == BATCH_FILES || currentBytes+size > BATCH_BYTES {
			batches = append(batches, current)
			current, currentBytes = nil, 0
		}
		current = append(current, j)
		currentBytes += size
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// hashBatch hashes the files of jobs on both nodes concurrently, and returns per job whether
// the hashes are equal and the errors on each side.
func hashBatch(nodeA, nodeB DirNode, jobs []CompareJob, limits []int64, followSym bool) ([]bool, []error, []error) {
	itemsA, itemsB := make([]HashItem, len(jobs)), make([]HashItem, len(jobs))
	for i, j := range jobs {
		itemsA[i] = HashItem{RelPath: j.PathA, Limit: limits[i]}
		itemsB[i] = HashItem{RelPath: j.PathB, Limit: limits[i]}
	}
	var hashesA []string
	var errsA []error
	done := make(chan struct{})
	go func() {
		defer close(done)
		hashesA, errsA = nodeA.GetSHAs(itemsA, followSym)
	}()
	hashesB, errsB := nodeB.GetSHAs(itemsB, followSym)
	<-done

	equal := make([]bool, len(jobs))
	for i := range jobs {
		equal[i] = errsA[i] == nil && errsB[i] == nil && hashesA[i] == hashesB[i]
	}
	return equal, errsA, errsB
}
