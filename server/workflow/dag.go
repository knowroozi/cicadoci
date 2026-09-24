// Package workflow: DAG resolution.
//
// This file implements the job DAG: building it from `needs` edges,
// detecting cycles at validation time, and computing fan-out/fan-in
// scheduling levels plus failure-propagation semantics for dependents.
package workflow

import (
	"fmt"
	"sort"
)

// DAG is a directed acyclic graph of jobs within a single workflow.
//
// Nodes are job names; an edge A→B means "A needs B" (B must complete
// before A can start).
type DAG struct {
	// Jobs is the set of job names in the DAG, in topological order.
	Jobs []string
	// edges[job] is the set of direct prerequisites for job.
	edges map[string]map[string]bool
	// revEdges[job] is the set of direct dependents of job.
	revEdges map[string]map[string]bool
	// levels[job] is the topological depth (0 = no dependencies).
	levels map[string]int
}

// BuildDAG constructs a DAG from a parsed workflow.
//
// It returns a *ValidationError if any job has a cycle, an unknown job
// referenced in `needs`, or a self-reference.
func BuildDAG(w *Workflow) (*DAG, error) {
	if w == nil {
		return nil, fmt.Errorf("nil workflow")
	}

	edges := make(map[string]map[string]bool, len(w.Jobs))
	revEdges := make(map[string]map[string]bool, len(w.Jobs))
	levels := make(map[string]int, len(w.Jobs))

	// Collect all job names for unknown-reference detection.
	knownJobs := make(map[string]bool, len(w.Jobs))
	for name := range w.Jobs {
		knownJobs[name] = true
		edges[name] = make(map[string]bool)
		revEdges[name] = make(map[string]bool)
	}

	// Build forward and reverse edges from each job's `needs` list.
	for name, job := range w.Jobs {
		for _, dep := range job.Needs {
			if !knownJobs[dep] {
				return nil, &ValidationError{errs: []error{fmt.Errorf(
					"job %q needs unknown job %q", name, dep)}}
			}
			if dep == name {
				return nil, &ValidationError{errs: []error{fmt.Errorf(
					"job %q has a self-referencing 'needs' entry", name)}}
			}
			edges[name][dep] = true
			revEdges[dep][name] = true
		}
	}

	// Compute topological levels via Kahn's algorithm (modified for level
	// tracking).
	//
	// Level 0: jobs with no prerequisites.
	// Level N: level = max(level of all prerequisites) + 1.
	//
	// We process in "generations": each outer-loop iteration processes all
	// jobs at the current level simultaneously (this is the fan-out level).
	// The `undep` map holds the remaining unresolved prerequisite count for
	// each job (equivalent to in-degree in the prerequisite graph).

	undep := make(map[string]int, len(knownJobs))
	processed := make(map[string]bool, len(knownJobs))
	for name := range knownJobs {
		undep[name] = len(edges[name])
		levels[name] = -1
	}

	var order []string
	remaining := len(knownJobs)
	level := 0

	for remaining > 0 {
		// Collect all jobs at the current level (undep == 0, not yet processed).
		var zero []string
		for name, count := range undep {
			if count == 0 && !processed[name] {
				zero = append(zero, name)
			}
		}
		if len(zero) == 0 {
			break // No job can start — there is a residual cycle.
		}
		for _, name := range zero {
			levels[name] = level
			processed[name] = true
			order = append(order, name)
			remaining--
			// Decrement undep for each dependent of this job.
			for dep := range revEdges[name] {
				undep[dep]--
			}
		}
		level++
	}

	if remaining > 0 {
		// Find the jobs that are in a cycle (those not yet processed).
		var cyclic []string
		for name := range knownJobs {
			if !processed[name] {
				cyclic = append(cyclic, name)
			}
		}
		sort.Strings(cyclic)
		return nil, &ValidationError{errs: []error{fmt.Errorf(
			"cycle detected involving jobs: %v", cyclic)}}
	}

	sort.Strings(order)

	return &DAG{
		Jobs:     order,
		edges:    edges,
		revEdges: revEdges,
		levels:   levels,
	}, nil
}

// Level returns the topological level of a job (0 = no dependencies).
// Returns -1 for unknown jobs or empty DAGs.
func (d *DAG) Level(job string) int {
	return d.levels[job]
}

// Needs returns the sorted direct prerequisite job names for a given job.
func (d *DAG) Needs(job string) []string {
	out := make([]string, 0, len(d.edges[job]))
	for dep := range d.edges[job] {
		out = append(out, dep)
	}
	sort.Strings(out)
	return out
}

// Dependents returns the sorted job names that directly depend on the given job.
func (d *DAG) Dependents(job string) []string {
	out := make([]string, 0, len(d.revEdges[job]))
	for dep := range d.revEdges[job] {
		out = append(out, dep)
	}
	sort.Strings(out)
	return out
}

// TransitiveDeps returns the full set of transitive prerequisite job names
// for a given job (all jobs that must complete before it can start),
// sorted alphabetically.
func (d *DAG) TransitiveDeps(job string) []string {
	seen := make(map[string]bool)
	var out []string
	var stack []string
	stack = append(stack, job)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for _, dep := range d.Needs(cur) {
			out = append(out, dep)
			stack = append(stack, dep)
		}
	}
	sort.Strings(out)
	return out
}

// FanOutLevel returns the sorted set of jobs at a given topological level.
// These jobs can all start simultaneously.
func (d *DAG) FanOutLevel(level int) []string {
	var out []string
	for job, lvl := range d.levels {
		if lvl == level {
			out = append(out, job)
		}
	}
	sort.Strings(out)
	return out
}

// MaxLevel returns the highest topological level in the DAG, or -1 if the
// DAG is empty.
func (d *DAG) MaxLevel() int {
	m := -1
	for _, lvl := range d.levels {
		if lvl > m {
			m = lvl
		}
	}
	return m
}

// PropagateFailure computes the set of jobs that should be marked "skipped"
// because one or more of their prerequisites failed or were cancelled.
//
// The `failedJobs` map lists all jobs that completed with a non-success
// status (failed, cancelled, timed out, etc.).
//
// A dependent job is skipped UNLESS its first step has `if: always` —
// in which case the job runs regardless of its prerequisites (the common
// CI/CD "always run" convention for notification/cleanup steps).
//
// The returned slice does not include the failed jobs themselves; it is the
// set of jobs that are transitively impacted by the failure.
func (d *DAG) PropagateFailure(w *Workflow, failedJobs map[string]bool) []string {
	if len(failedJobs) == 0 {
		return nil
	}

	skipped := make(map[string]bool)

	// BFS from each failed/impact-affected job: propagate to dependents.
	var queue []string
	for job := range failedJobs {
		queue = append(queue, job)
	}
	sort.Strings(queue)

	for i := 0; i < len(queue); i++ {
		failed := queue[i]
		for _, dependent := range d.Dependents(failed) {
			if skipped[dependent] {
				continue
			}
			// Check: does this dependent have `if: always` on its first step?
			// If so, it runs regardless of upstream status.
			job := w.Jobs[dependent]
			if job != nil && len(job.Steps) > 0 && job.Steps[0].If == "always" {
				continue
			}
			skipped[dependent] = true
			queue = append(queue, dependent)
		}
	}

	out := make([]string, 0, len(skipped))
	for job := range skipped {
		out = append(out, job)
	}
	sort.Strings(out)
	return out
}

// FanInReady returns the sorted set of job names whose all prerequisites
// are in the `completed` map (i.e. a fan-in event that unblocks the job).
//
// A job with no prerequisites is always considered "ready".
//
// This method is the core of the fan-in scheduling logic: the caller
// repeatedly calls FanInReady as complete jobs are added to `completed`
// to determine which jobs to enqueue next.
func (d *DAG) FanInReady(completed map[string]bool) []string {
	var ready []string
	for name := range d.levels {
		if len(d.edges[name]) == 0 {
			// Job with no prerequisites: always ready.
			ready = append(ready, name)
		} else {
			allComplete := true
			for dep := range d.edges[name] {
				if !completed[dep] {
					allComplete = false
					break
				}
			}
			if allComplete {
				ready = append(ready, name)
			}
		}
	}
	sort.Strings(ready)
	return ready
}
