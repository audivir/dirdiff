package main

import (
	"context"
	"path"
	"slices"
	"sync"
)

const SHORT_HASH_LEN = 7

// flatCompare matches files by name, ignoring directories.
// A name found once on each side becomes a compare job. Copies of a name found more than once on a
// side are paired by content hash, and unpaired copies are reported with a short hash.
func flatCompare(
	ctx context.Context,
	filesA, filesB map[string]int64,
	nodeA, nodeB DirNode,
	hashLimit func(string) int64,
	followSym bool,
	workers int,
) ([]DiffItem, []CompareJob, []ReadFailure) {
	byNameA, byNameB := groupByName(filesA), groupByName(filesB)

	var results []DiffItem
	var jobs []CompareJob
	var dupNames []string
	var tasks []hashTask
	for name, pathsA := range byNameA {
		pathsB := byNameB[name]
		switch {
		case len(pathsB) == 0:
			for _, p := range pathsA {
				results = append(results, DiffItem{Path: p, Type: Removed})
			}
		case len(pathsA) == 1 && len(pathsB) == 1:
			jobs = append(jobs, CompareJob{PathA: pathsA[0], PathB: pathsB[0]})
		default:
			dupNames = append(dupNames, name)
			for _, p := range pathsA {
				tasks = append(tasks, hashTask{side: "A", node: nodeA, path: p})
			}
			for _, p := range pathsB {
				tasks = append(tasks, hashTask{side: "B", node: nodeB, path: p})
			}
		}
	}
	for name, pathsB := range byNameB {
		if _, ok := byNameA[name]; !ok {
			for _, p := range pathsB {
				results = append(results, DiffItem{Path: p, Type: Added})
			}
		}
	}

	hashes, failures := hashAll(ctx, tasks, hashLimit, followSym, workers)

	for _, name := range dupNames {
		byHashA := groupByHash(byNameA[name], hashes["A"])
		byHashB := groupByHash(byNameB[name], hashes["B"])
		for hash, pathsA := range byHashA {
			paired := min(len(pathsA), len(byHashB[hash]))
			for _, p := range pathsA[paired:] {
				results = append(results, DiffItem{Path: p, Type: Removed, Hash: hash[:SHORT_HASH_LEN]})
			}
		}
		for hash, pathsB := range byHashB {
			paired := min(len(pathsB), len(byHashA[hash]))
			for _, p := range pathsB[paired:] {
				results = append(results, DiffItem{Path: p, Type: Added, Hash: hash[:SHORT_HASH_LEN]})
			}
		}
	}
	return results, jobs, failures
}

// hashTask stores a file to hash on one side.
type hashTask struct {
	side string
	node DirNode
	path string
}

// hashAll hashes the files of tasks in parallel and returns their hashes by side and path.
func hashAll(
	ctx context.Context,
	tasks []hashTask,
	hashLimit func(string) int64,
	followSym bool,
	workers int,
) (map[string]map[string]string, []ReadFailure) {
	hashes := map[string]map[string]string{"A": {}, "B": {}}
	var failures []ReadFailure
	var mu sync.Mutex

	taskCh := make(chan hashTask, len(tasks))
	for _, t := range tasks {
		taskCh <- t
	}
	close(taskCh)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for t := range taskCh {
				if ctx.Err() != nil {
					return
				}
				hash, err := t.node.GetSHA(t.path, hashLimit(t.path), followSym)
				mu.Lock()
				if err != nil {
					failures = append(failures, ReadFailure{Path: t.path, Msg: t.side + ": " + err.Error()})
				} else {
					hashes[t.side][t.path] = hash
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return hashes, failures
}

// groupByName maps file names to their sorted paths.
func groupByName(files map[string]int64) map[string][]string {
	byName := make(map[string][]string)
	for p := range files {
		byName[path.Base(p)] = append(byName[path.Base(p)], p)
	}
	for _, paths := range byName {
		slices.Sort(paths)
	}
	return byName
}

// groupByHash maps hashes to the paths with that hash, skipping paths that could not be hashed.
func groupByHash(paths []string, hashes map[string]string) map[string][]string {
	byHash := make(map[string][]string)
	for _, p := range paths {
		if hash, ok := hashes[p]; ok {
			byHash[hash] = append(byHash[hash], p)
		}
	}
	return byHash
}
