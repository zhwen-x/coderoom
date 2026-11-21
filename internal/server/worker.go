package server

import (
	"context"
	"fmt"
	"time"

	"coderoom/internal/judge"
	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

const (
	idlePoll  = 500 * time.Millisecond
	errorPoll = 2 * time.Second
	// A single submission must not be able to wedge a worker forever.
	taskTimeout = 5 * time.Minute
)

// judgeWorker pulls tasks off the persisted queue until ctx is cancelled.
// Because the queue lives in the database rather than in memory, a server
// crash loses no work: RequeueStaleTasks at startup puts interrupted tasks
// back to pending.
func (s *Server) judgeWorker(ctx context.Context, name string) {
	for {
		if ctx.Err() != nil {
			return
		}
		task, err := s.st.ClaimTask(ctx, name)
		if err != nil {
			s.logger.Printf("[%s] claim task: %v", name, err)
			if !sleepCtx(ctx, errorPoll) {
				return
			}
			continue
		}
		if task == nil {
			if !sleepCtx(ctx, idlePoll) {
				return
			}
			continue
		}
		s.runTask(ctx, name, task)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (s *Server) runTask(ctx context.Context, worker string, task *store.JudgeTask) {
	taskCtx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()

	fail := func(msg string) {
		// A judge-side failure still gets a stored result, so the teacher sees
		// "SE: <reason>" in the gradebook rather than a silently missing row.
		_ = s.st.SaveJudgeResult(taskCtx, &store.JudgeResult{
			SubmissionID: task.SubmissionID,
			Verdict:      protocol.VerdictSE,
			CompileLog:   msg,
			Cases:        []protocol.CaseResult{},
		})
		_ = s.st.FinishTask(taskCtx, task.ID, store.TaskError, msg)
		s.logger.Printf("[%s] submission %d: %s", worker, task.SubmissionID, msg)
	}

	sub, err := s.st.GetSubmission(taskCtx, task.SubmissionID)
	if err != nil {
		fail("找不到提交记录: " + err.Error())
		return
	}
	problem, err := s.st.GetProblem(taskCtx, sub.ProblemID)
	if err != nil {
		fail("找不到题目: " + err.Error())
		return
	}
	cases, err := s.st.ListTestcases(taskCtx, sub.ProblemID)
	if err != nil {
		fail("读取测试数据失败: " + err.Error())
		return
	}
	code, err := s.st.ReadSubmissionCode(sub)
	if err != nil {
		fail("读取提交代码失败: " + err.Error())
		return
	}

	jcases := make([]judge.TestCase, 0, len(cases))
	for _, c := range cases {
		jcases = append(jcases, judge.TestCase{
			Index:      c.Idx,
			InputPath:  c.InputPath,
			OutputPath: c.OutputPath,
			Score:      c.Score,
		})
	}

	res, err := s.judger.Judge(taskCtx, judge.Request{
		Language: sub.Language,
		Code:     code,
		Problem: judge.Problem{
			TimeLimitMS:   problem.TimeLimitMS,
			MemoryLimitMB: problem.MemoryLimitMB,
			ScoreMode:     problem.ScoreMode,
		},
		Cases: jcases,
	})
	if err != nil {
		fail("判题失败: " + err.Error())
		return
	}

	if err := s.st.SaveJudgeResult(taskCtx, &store.JudgeResult{
		SubmissionID: sub.ID,
		Verdict:      res.Verdict,
		Score:        res.Score,
		MaxScore:     res.MaxScore,
		TimeMS:       res.TimeMS,
		MemoryKB:     res.MemoryKB,
		CompileLog:   res.CompileLog,
		Cases:        res.Cases,
	}); err != nil {
		fail("保存判题结果失败: " + err.Error())
		return
	}
	if err := s.st.FinishTask(taskCtx, task.ID, store.TaskDone, ""); err != nil {
		s.logger.Printf("[%s] finish task %d: %v", worker, task.ID, err)
	}
	s.logger.Printf("[%s] 判题完成 submission=%d %s %s %d/%d %dms",
		worker, sub.ID, sub.StudentNo, res.Verdict, res.Score, res.MaxScore, res.TimeMS)
}

// judgeStatus is the queue summary shown on the teacher's system panel.
func (s *Server) judgeStatus(ctx context.Context) string {
	n, err := s.st.PendingCount(ctx)
	if err != nil {
		return "未知"
	}
	if n == 0 {
		return "空闲"
	}
	return fmt.Sprintf("待判 %d", n)
}
