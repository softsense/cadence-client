// Copyright (c) 2017-2021 Uber Technologies Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package internal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally"
	"go.uber.org/zap/zaptest"

	"go.uber.org/cadence/.gen/go/cadence/workflowservicetest"
	s "go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/internal/common"
	"go.uber.org/cadence/internal/common/metrics"
)

const (
	_testDomainName = "test-domain"
	_testTaskList   = "test-tasklist"
	_testIdentity   = "test-worker"
)

func TestLocalActivityPanic(t *testing.T) {
	// regression: panics in local activities should not terminate the process
	s := WorkflowTestSuite{logger: zaptest.NewLogger(t)}
	env := s.NewTestWorkflowEnvironment()

	wf := "panicky_local_activity"
	env.RegisterWorkflowWithOptions(func(ctx Context) error {
		ctx = WithLocalActivityOptions(ctx, LocalActivityOptions{
			ScheduleToCloseTimeout: time.Second,
		})
		return ExecuteLocalActivity(ctx, func(ctx context.Context) error {
			panic("should not kill process")
		}).Get(ctx, nil)
	}, RegisterWorkflowOptions{Name: wf})

	env.ExecuteWorkflow(wf)
	err := env.GetWorkflowError()
	require.Error(t, err)
	var perr *PanicError
	require.True(t, errors.As(err, &perr), "error should be a panic error")
	assert.Contains(t, perr.StackTrace(), "panic")
	assert.Contains(t, perr.StackTrace(), t.Name(), "should mention the source location of the local activity that panicked")
}

func TestRespondTaskCompleted_failed(t *testing.T) {
	t.Run("fail sends RespondDecisionTaskFailedRequest", func(t *testing.T) {
		testTaskToken := []byte("test-task-token")

		poller, client, _, _ := buildWorkflowTaskPoller(t)
		client.EXPECT().RespondDecisionTaskFailed(gomock.Any(), &s.RespondDecisionTaskFailedRequest{
			TaskToken:      testTaskToken,
			Cause:          s.DecisionTaskFailedCauseWorkflowWorkerUnhandledFailure.Ptr(),
			Details:        []byte(assert.AnError.Error()),
			Identity:       common.StringPtr(_testIdentity),
			BinaryChecksum: common.StringPtr(getBinaryChecksum()),
		}, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

		res, err := poller.RespondTaskCompletedWithMetrics(nil, assert.AnError, &s.PollForDecisionTaskResponse{
			TaskToken: testTaskToken,
			Attempt:   common.Int64Ptr(0),
		}, time.Now())
		assert.NoError(t, err)
		assert.Nil(t, res)
	})
	t.Run("fail fails to send RespondDecisionTaskFailedRequest", func(t *testing.T) {
		testTaskToken := []byte("test-task-token")

		poller, client, _, _ := buildWorkflowTaskPoller(t)
		client.EXPECT().RespondDecisionTaskFailed(gomock.Any(), &s.RespondDecisionTaskFailedRequest{
			TaskToken:      testTaskToken,
			Cause:          s.DecisionTaskFailedCauseWorkflowWorkerUnhandledFailure.Ptr(),
			Details:        []byte(assert.AnError.Error()),
			Identity:       common.StringPtr(_testIdentity),
			BinaryChecksum: common.StringPtr(getBinaryChecksum()),
		}, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(assert.AnError)

		// We cannot test RespondTaskCompleted since it uses backoff and has a hardcoded retry mechanism for 60 seconds.
		_, err := poller.respondTaskCompletedAttempt(errorToFailDecisionTask(testTaskToken, assert.AnError, _testIdentity), &s.PollForDecisionTaskResponse{
			TaskToken: testTaskToken,
			Attempt:   common.Int64Ptr(0),
		})
		assert.ErrorIs(t, err, assert.AnError)
	})
	t.Run("fail skips sending for not the first attempt", func(t *testing.T) {
		poller, _, _, _ := buildWorkflowTaskPoller(t)

		res, err := poller.RespondTaskCompletedWithMetrics(nil, assert.AnError, &s.PollForDecisionTaskResponse{
			Attempt: common.Int64Ptr(1),
		}, time.Now())
		assert.NoError(t, err)
		assert.Nil(t, res)
	})
}

func TestRespondTaskCompleted_Unsupported(t *testing.T) {
	poller, _, _, _ := buildWorkflowTaskPoller(t)

	assert.PanicsWithValue(t, "unknown request type from ProcessWorkflowTask()", func() {
		_, _ = poller.RespondTaskCompletedWithMetrics(assert.AnError, nil, &s.PollForDecisionTaskResponse{}, time.Now())
	})
}

func TestProcessTask_failures(t *testing.T) {
	t.Run("shutdown", func(t *testing.T) {
		poller, _, _, _ := buildWorkflowTaskPoller(t)
		ch := make(chan struct{})
		poller.shutdownC = ch
		close(ch)

		err := poller.ProcessTask(&workflowTask{})
		assert.ErrorIs(t, err, errShutdown)
	})
	t.Run("unsupported task type", func(t *testing.T) {
		poller, _, _, _ := buildWorkflowTaskPoller(t)
		assert.PanicsWithValue(t, "unknown task type.", func() {
			_ = poller.ProcessTask(10)
		})
	})
	t.Run("nil task", func(t *testing.T) {
		poller, _, _, _ := buildWorkflowTaskPoller(t)

		err := poller.ProcessTask(&workflowTask{})
		assert.NoError(t, err)
	})
	t.Run("heartbeat error", func(t *testing.T) {
		poller, _, mockedTaskHandler, _ := buildWorkflowTaskPoller(t)
		hearbeatErr := &decisionHeartbeatError{}
		mockedTaskHandler.EXPECT().ProcessWorkflowTask(mock.Anything, mock.Anything).Return(nil, hearbeatErr)
		err := poller.ProcessTask(&workflowTask{
			task: &s.PollForDecisionTaskResponse{},
		})
		assert.ErrorIs(t, err, hearbeatErr)
	})
	t.Run("ResetStickyTaskList fail", func(t *testing.T) {
		poller, client, _, _ := buildWorkflowTaskPoller(t)
		client.EXPECT().ResetStickyTaskList(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, assert.AnError)
		err := poller.ProcessTask(&resetStickinessTask{
			task: &s.ResetStickyTaskListRequest{
				Execution: &s.WorkflowExecution{
					WorkflowId: common.StringPtr("test-workflow-id"),
					RunId:      common.StringPtr("test-run-id"),
				},
			},
		})
		assert.ErrorIs(t, err, assert.AnError)
	})
}

func buildWorkflowTaskPoller(t *testing.T) (*workflowTaskPoller, *workflowservicetest.MockClient, *MockWorkflowTaskHandler, *mockLocalDispatcher) {
	ctrl := gomock.NewController(t)
	mockService := workflowservicetest.NewMockClient(ctrl)
	taskHandler := &MockWorkflowTaskHandler{}
	lda := &mockLocalDispatcher{}

	return &workflowTaskPoller{
		basePoller: basePoller{
			shutdownC: make(<-chan struct{}),
		},
		domain:                       _testDomainName,
		taskListName:                 _testTaskList,
		identity:                     _testIdentity,
		service:                      mockService,
		taskHandler:                  taskHandler,
		ldaTunnel:                    lda,
		metricsScope:                 &metrics.TaggedScope{Scope: tally.NewTestScope("test", nil)},
		logger:                       zaptest.NewLogger(t),
		stickyUUID:                   "",
		disableStickyExecution:       false,
		StickyScheduleToStartTimeout: time.Millisecond,
		featureFlags:                 FeatureFlags{},
	}, mockService, taskHandler, lda
}
