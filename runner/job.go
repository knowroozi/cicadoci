package runner

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/runner/executor"
)

// infraErrorClass labels a StatusUpdate the runner sends when a job could not
// even be prepared or its executor built — an infrastructure failure, as opposed
// to a code failure inside a command that did run. Kept as one const because
// every such block reports exactly this class (goconst would otherwise flag the
// repeated literal).
const infraErrorClass = "infra"

func runtimeArch() string { return runtime.GOARCH }

// startJob acknowledges an assignment and runs it in the background.
//
// The ack goes out before execution starts: the server needs to know quickly
// whether this runner took the job, and a slow Prepare must not look like a
// silent drop.
func (a *Agent) startJob(ctx context.Context, assignment *runnerv1.JobAssignment, send func(*runnerv1.RunnerMessage) error) {
	jobID := assignment.GetJobId()
	leaseID := assignment.GetLeaseId()

	a.jobsMu.Lock()
	if _, running := a.active[jobID]; running {
		a.jobsMu.Unlock()
		// A redelivered assignment for a job we are already running. Ack it and
		// carry on: assignments are redeliverable by design.
		_ = send(ackMessage(jobID, leaseID, runnerv1.JobAck_ACCEPTED, ""))
		return
	}
	// Soft backpressure: a runner at capacity rejects a new assignment outright
	// rather than queueing it locally, so the dispatcher's Requeue puts the job
	// back where another runner (or this one, once it frees up) can claim it.
	// There is no partial admission — a runner exactly at capacity accepts
	// nothing more until something finishes. That's a fine story for the shell
	// executor's one-job-at-a-time-in-practice usage; a docker executor with
	// higher real concurrency may want softer throttling than a hard reject.
	if len(a.active) >= a.cfg.Capacity {
		a.jobsMu.Unlock()
		a.log.Warn("rejecting job: at capacity", "job_id", jobID)
		_ = send(ackMessage(jobID, leaseID, runnerv1.JobAck_REJECTED, "capacity"))
		return
	}

	spec := assignment.GetSpec()
	if spec.GetExecutor() != "" && !a.supportsExecutor(spec.GetExecutor()) {
		a.jobsMu.Unlock()
		a.log.Warn("rejecting job: unsupported executor",
			"job_id", jobID, "executor", spec.GetExecutor())
		_ = send(ackMessage(jobID, leaseID, runnerv1.JobAck_REJECTED, "executor_unavailable"))
		return
	}

	jobCtx, cancel := context.WithCancel(ctx)
	if timeout := spec.GetTimeoutSeconds(); timeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	}
	a.active[jobID] = &activeJob{leaseID: leaseID, cancel: cancel}
	a.jobsMu.Unlock()

	if err := send(ackMessage(jobID, leaseID, runnerv1.JobAck_ACCEPTED, "")); err != nil {
		a.log.Error("send job ack", "job_id", jobID, "error", err)
	}

	go func() {
		defer cancel()
		defer a.finishJob(jobID)
		a.executeJob(jobCtx, assignment, send)
	}()
}

func (a *Agent) supportsExecutor(name string) bool {
	for _, e := range a.cfg.Executors {
		if e == name {
			return true
		}
	}
	return false
}

// executeJob runs every step of a job, streaming output and reporting status.
func (a *Agent) executeJob(ctx context.Context, assignment *runnerv1.JobAssignment, send func(*runnerv1.RunnerMessage) error) {
	jobID := assignment.GetJobId()
	leaseID := assignment.GetLeaseId()
	spec := assignment.GetSpec()

	a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
		State: runnerv1.StatusUpdate_PREPARING,
	})

	exec, err := executor.NewExecutor(spec.GetExecutor())
	if err != nil {
		a.log.Error("unknown executor", "job_id", jobID, "executor", spec.GetExecutor(), "error", err)
		a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
			State:      runnerv1.StatusUpdate_FAILED,
			ExitCode:   -1,
			ErrorClass: infraErrorClass,
			Detail:     err.Error(),
		})
		return
	}
	job := executor.Job{
		ID:            jobID,
		RunID:         spec.GetRunId(),
		WorkspaceRoot: a.cfg.WorkspaceRoot,
		Env:           spec.GetEnv(),
	}

	if err := exec.Prepare(ctx, job); err != nil {
		a.log.Error("prepare job", "job_id", jobID, "error", err)
		a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
			State:      runnerv1.StatusUpdate_FAILED,
			ExitCode:   -1,
			ErrorClass: infraErrorClass,
			Detail:     err.Error(),
		})
		return
	}
	defer func() {
		// Cleanup must run even when the job was cancelled, so it gets a fresh
		// context rather than the cancelled one.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := exec.Cleanup(cleanupCtx); err != nil {
			a.log.Warn("cleanup workspace", "job_id", jobID, "error", err)
		}
	}()

	for _, step := range spec.GetSteps() {
		stepIndex := step.GetIndex()

		a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
			State:     runnerv1.StatusUpdate_RUNNING_STEP,
			StepIndex: stepIndex,
		})
		a.sendEvent(jobID, leaseID, stepIndex, "step_started",
			fmt.Sprintf(`{"name":%q,"image":%q}`, step.GetName(), step.GetImage()))

		sink := executor.OutputSinkFunc(func(stream executor.Stream, data []byte) {
			a.sendChunk(jobID, leaseID, stepIndex, stream, data)
		})

		result, err := exec.RunStep(ctx, executor.Step{
			Index:          int32(stepIndex),
			Name:           step.GetName(),
			Image:          step.GetImage(),
			Command:        step.GetCommand(),
			Env:            mergeEnv(spec.GetEnv(), step.GetEnv()),
			Timeout:        time.Duration(step.GetTimeoutSeconds()) * time.Second,
			NetworkEnabled: step.GetNetworkEnabled(),
		}, sink)

		switch {
		case err != nil:
			// The step could not be run at all: infrastructure, not the job's fault.
			a.sendEvent(jobID, leaseID, stepIndex, "error_classified",
				fmt.Sprintf(`{"class":"infra","detail":%q}`, err.Error()))
			a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
				State:      runnerv1.StatusUpdate_FAILED,
				StepIndex:  stepIndex,
				ExitCode:   -1,
				ErrorClass: infraErrorClass,
				Detail:     err.Error(),
			})
			return

		case ctx.Err() != nil:
			a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
				State:     runnerv1.StatusUpdate_CANCELLED,
				StepIndex: stepIndex,
			})
			return

		case result.ExitCode != 0:
			errorClass := "user_script"
			if result.TimedOut {
				errorClass = "timeout"
			}
			a.sendEvent(jobID, leaseID, stepIndex, "step_finished",
				fmt.Sprintf(`{"exit_code":%d,"duration_ms":%d,"timed_out":%t}`,
					result.ExitCode, result.Duration.Milliseconds(), result.TimedOut))
			a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
				State:      runnerv1.StatusUpdate_FAILED,
				StepIndex:  stepIndex,
				ExitCode:   result.ExitCode,
				ErrorClass: errorClass,
			})
			return

		default:
			a.sendEvent(jobID, leaseID, stepIndex, "step_finished",
				fmt.Sprintf(`{"exit_code":0,"duration_ms":%d}`, result.Duration.Milliseconds()))
		}
	}

	a.sendStatus(send, jobID, leaseID, &runnerv1.StatusUpdate{
		State:    runnerv1.StatusUpdate_SUCCEEDED,
		ExitCode: 0,
	})
}

func mergeEnv(jobEnv, stepEnv map[string]string) map[string]string {
	merged := make(map[string]string, len(jobEnv)+len(stepEnv))
	for k, v := range jobEnv {
		merged[k] = v
	}
	for k, v := range stepEnv {
		merged[k] = v
	}
	return merged
}

func ackMessage(jobID, leaseID string, result runnerv1.JobAck_Result, reason string) *runnerv1.RunnerMessage {
	return &runnerv1.RunnerMessage{
		Msg: &runnerv1.RunnerMessage_JobAck{JobAck: &runnerv1.JobAck{
			JobId:        jobID,
			LeaseId:      leaseID,
			Result:       result,
			RejectReason: reason,
		}},
	}
}

// sendStatus fills in the identity and sequence fields and sends a status update.
//
// seq is monotonic per runner rather than per job. The protocol requires it to
// be monotonic per job and uses (job_id, seq) as the upsert key, so a
// runner-wide counter satisfies that with room to spare and avoids a per-job
// counter that would need to survive a reconnect.
func (a *Agent) sendStatus(send func(*runnerv1.RunnerMessage) error, jobID, leaseID string, upd *runnerv1.StatusUpdate) {
	upd.JobId = jobID
	upd.LeaseId = leaseID
	upd.Seq = a.statusSeq.Add(1)
	upd.RunnerTs = timestamppb.Now()

	if err := send(&runnerv1.RunnerMessage{
		Msg: &runnerv1.RunnerMessage_StatusUpdate{StatusUpdate: upd},
	}); err != nil {
		a.log.Error("send status update", "job_id", jobID, "state", upd.GetState(), "error", err)
	}
}

// nextSeq allocates the next sequence number in a job's shared log/event space.
func (a *Agent) nextSeq(jobID string) uint64 {
	a.seqMu.Lock()
	counter, ok := a.seq[jobID]
	if !ok {
		counter = &atomic.Uint64{}
		a.seq[jobID] = counter
	}
	a.seqMu.Unlock()
	return counter.Add(1)
}

func (a *Agent) setLogStream(stream runnerv1.RunnerService_LogStreamClient) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	a.logStream = stream
}

// sendLogUp writes to the bulk lane under a lock: a gRPC stream must not be
// written from two goroutines, and steps produce output concurrently.
//
// Every job's pump goroutines contend on this one lock and one LogStream RPC.
// At M0 that's fine — one runner, one job, one capacity — but it means
// concurrent jobs on a single runner (once capacity > 1 is exercised, or the
// docker executor lands) will serialize their log writes through here. If
// that shows up as a bottleneck, the fix is a per-job channel feeding a
// single writer goroutine per LogStream, not more locks.
func (a *Agent) sendLogUp(up *runnerv1.LogUp) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.logStream == nil {
		return
	}
	if err := a.logStream.Send(up); err != nil {
		a.log.Debug("send on log stream", "error", err)
	}
}

func (a *Agent) sendChunk(jobID, leaseID string, stepIndex uint32, stream executor.Stream, data []byte) {
	io := runnerv1.LogChunk_STDOUT
	if stream == executor.StreamStderr {
		io = runnerv1.LogChunk_STDERR
	}
	a.sendLogUp(&runnerv1.LogUp{
		Msg: &runnerv1.LogUp_Chunk{Chunk: &runnerv1.LogChunk{
			JobId:     jobID,
			LeaseId:   leaseID,
			StepIndex: stepIndex,
			Io:        io,
			Seq:       a.nextSeq(jobID),
			Data:      data,
			RunnerTs:  timestamppb.Now(),
		}},
	})
}

func (a *Agent) sendEvent(jobID, leaseID string, stepIndex uint32, kind, payload string) {
	a.sendLogUp(&runnerv1.LogUp{
		Msg: &runnerv1.LogUp_Event{Event: &runnerv1.StructuredEvent{
			JobId:     jobID,
			LeaseId:   leaseID,
			StepIndex: stepIndex,
			Seq:       a.nextSeq(jobID),
			Kind:      kind,
			Json:      payload,
		}},
	})
}

// finishJob releases everything keyed on jobID once a job is done.
//
// This is also what bounds a.seq's size: it is never cleared wholesale on
// reconnect, only per job here, so its footprint tracks the runner's active
// (plus recently-finished-but-not-yet-cleaned-up) jobs rather than its
// lifetime job count.
func (a *Agent) finishJob(jobID string) {
	a.jobsMu.Lock()
	delete(a.active, jobID)
	a.jobsMu.Unlock()

	a.seqMu.Lock()
	delete(a.seq, jobID)
	a.seqMu.Unlock()
}

func (a *Agent) activeJobIDs() []string {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	ids := make([]string, 0, len(a.active))
	for id := range a.active {
		ids = append(ids, id)
	}
	return ids
}

// cancelJob kills local execution of a job the server has taken back.
func (a *Agent) cancelJob(cancel *runnerv1.CancelJob) {
	jobID := cancel.GetJobId()

	a.jobsMu.Lock()
	job, ok := a.active[jobID]
	a.jobsMu.Unlock()
	if !ok {
		return
	}

	a.log.Info("cancelling job", "job_id", jobID, "reason", cancel.GetReason())
	if grace := cancel.GetGraceSeconds(); grace > 0 {
		time.AfterFunc(time.Duration(grace)*time.Second, job.cancel)
		return
	}
	job.cancel()
}

// resumeStates reports the jobs this runner believes it is still executing, for
// Hello. On a fresh start it is empty.
//
// LastStatusSeq is a.statusSeq — the runner-wide counter, not a per-job one —
// so it over-reports the true per-job watermark whenever more than one job is
// active (see Agent.statusSeq). That's informational only in this protocol
// version: the server's resumeVerdicts derives log/status watermarks from its
// own DB-stored jobs.log_watermark/status_watermark, not from anything the
// runner claims here. Worth fixing if a future server implementation ever
// starts trusting this field, but there is nothing to fix in the wire
// contract today — LastStatusSeq exists per-job in the message precisely so a
// server could use it that way later.
func (a *Agent) resumeStates() []*runnerv1.ResumeState {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()

	states := make([]*runnerv1.ResumeState, 0, len(a.active))
	for jobID, job := range a.active {
		states = append(states, &runnerv1.ResumeState{
			JobId:         jobID,
			LeaseId:       job.leaseID,
			LastStatusSeq: a.statusSeq.Load(),
			LastLogSeq:    a.currentSeq(jobID),
		})
	}
	return states
}

func (a *Agent) currentSeq(jobID string) uint64 {
	a.seqMu.Lock()
	defer a.seqMu.Unlock()
	if counter, ok := a.seq[jobID]; ok {
		return counter.Load()
	}
	return 0
}

// applyResumeVerdicts acts on the server's answer to our Hello.
//
// ABANDON means the lease is gone: kill local execution and report nothing
// further about that job. Continuing to run it would mean two runners executing
// the same job, which for a deploy means deploying twice.
func (a *Agent) applyResumeVerdicts(verdicts []*runnerv1.ResumeVerdict) {
	for _, v := range verdicts {
		if v.GetVerdict() == runnerv1.ResumeVerdict_RESUME {
			a.log.Info("resuming job after reconnect",
				"job_id", v.GetJobId(),
				"log_watermark", v.GetLogWatermark(),
				"status_watermark", v.GetStatusWatermark())
			continue
		}

		a.log.Warn("abandoning job: lease is gone", "job_id", v.GetJobId())
		a.jobsMu.Lock()
		if job, ok := a.active[v.GetJobId()]; ok {
			job.cancel()
			delete(a.active, v.GetJobId())
		}
		a.jobsMu.Unlock()
	}
}
