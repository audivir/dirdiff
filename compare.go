package main

// BATCH_BYTES bounds the total size of the files hashed in one request.
const BATCH_BYTES = 32 * 1024 * 1024

// makeBatches groups jobs for the workers. With a positive batchSize, up to batchSize files of
// at most PRECHECK_SIZE are grouped, so that a remote node hashes many of them per request.
// Larger files and all files without batching run alone.
func makeBatches(jobs []CompareJob, sizes map[string]FileMeta, batchSize int) [][]CompareJob {
	var batches [][]CompareJob
	var current []CompareJob
	var currentBytes int64
	for _, j := range jobs {
		size := sizes[j.PathA].Size
		if batchSize <= 0 || size > PRECHECK_SIZE {
			batches = append(batches, []CompareJob{j})
			continue
		}
		if len(current) == batchSize || currentBytes+size > BATCH_BYTES {
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
