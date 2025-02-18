//go:build windows && unit
// +build windows,unit

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/aws/amazon-ecs-agent/agent/data"
	"github.com/aws/amazon-ecs-agent/agent/ec2"
	mock_dockerstate "github.com/aws/amazon-ecs-agent/agent/engine/dockerstate/mocks"
	mock_engine "github.com/aws/amazon-ecs-agent/agent/engine/mocks"
	"github.com/aws/amazon-ecs-agent/agent/eventstream"
	"github.com/aws/amazon-ecs-agent/agent/sighandlers"
	"github.com/aws/amazon-ecs-agent/agent/sighandlers/exitcodes"
	statemanager_mocks "github.com/aws/amazon-ecs-agent/agent/statemanager/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows/svc"
)

type mockAgent struct {
	startFunc          func() int
	terminationHandler sighandlers.TerminationHandler
}

func (m *mockAgent) start() int {
	return m.startFunc()
}
func (m *mockAgent) setTerminationHandler(handler sighandlers.TerminationHandler) {
	m.terminationHandler = handler
}
func (m *mockAgent) printECSAttributes() int   { return 0 }
func (m *mockAgent) startWindowsService() int  { return 0 }
func (m *mockAgent) getConfig() *config.Config { return &config.Config{} }

func TestHandler_RunAgent_StartExitImmediately(t *testing.T) {
	// register some mocks, but nothing should get called on any of them
	ctrl := gomock.NewController(t)
	_ = statemanager_mocks.NewMockStateManager(ctrl)
	_ = mock_engine.NewMockTaskEngine(ctrl)
	defer ctrl.Finish()

	wg := sync.WaitGroup{}
	wg.Add(1)
	startFunc := func() int {
		// startFunc doesn't block, nothing is called
		wg.Done()
		return 0
	}
	agent := &mockAgent{startFunc: startFunc}
	handler := &handler{agent}
	go handler.runAgent(context.TODO())
	wg.Wait()
	assert.NotNil(t, agent.terminationHandler)
}

func TestHandler_RunAgent_NoSaveWithNoTerminationHandler(t *testing.T) {
	// register some mocks, but nothing should get called on any of them
	ctrl := gomock.NewController(t)
	_ = statemanager_mocks.NewMockStateManager(ctrl)
	_ = mock_engine.NewMockTaskEngine(ctrl)
	defer ctrl.Finish()

	done := make(chan struct{})
	startFunc := func() int {
		<-done // block until after the test ends so that we can test that runAgent returns when cancelled
		return 0
	}
	agent := &mockAgent{startFunc: startFunc}
	handler := &handler{agent}
	ctx, cancel := context.WithCancel(context.TODO())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		handler.runAgent(ctx)
		wg.Done()
	}()
	cancel()
	wg.Wait()
	assert.NotNil(t, agent.terminationHandler)
}

func TestHandler_RunAgent_ForceSaveWithTerminationHandler(t *testing.T) {
	ctrl := gomock.NewController(t)
	taskEngine := mock_engine.NewMockTaskEngine(ctrl)
	taskEngineState := mock_dockerstate.NewMockTaskEngineState(ctrl)
	defer ctrl.Finish()
	dataClient := data.NewNoopClient()

	taskEngine.EXPECT().Disable()
	taskEngineState.EXPECT().AllTasks().Return(nil)
	taskEngineState.EXPECT().AllImageStates().Return(nil)
	taskEngineState.EXPECT().AllENIAttachments().Return(nil)
	taskEngineState.EXPECT().GetAllContainerIDs().Return([]string{"test-container"})
	taskEngineState.EXPECT().ContainerByID("test-container").Return(nil, false)

	agent := &mockAgent{}

	ctx, cancel := context.WithCancel(context.TODO())
	done := make(chan struct{})
	defer func() { done <- struct{}{} }()
	startFunc := func() int {
		go agent.terminationHandler(taskEngineState, dataClient, taskEngine, cancel)
		<-done // block until after the test ends so that we can test that runAgent returns when cancelled
		return 0
	}
	agent.startFunc = startFunc
	handler := &handler{agent}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		handler.runAgent(ctx)
		wg.Done()
	}()
	time.Sleep(time.Second) // give startFunc enough time to actually call the termination handler
	cancel()
	wg.Wait()
}

func TestHandler_HandleWindowsRequests_StopService(t *testing.T) {
	requests := make(chan svc.ChangeRequest)
	responses := make(chan svc.Status)

	handler := &handler{}
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		handler.handleWindowsRequests(context.TODO(), requests, responses)
		wg.Done()
	}()

	go func() {
		resp := <-responses
		assert.Equal(t, svc.StartPending, resp.State, "Send StartPending immediately")
		resp = <-responses
		assert.Equal(t, svc.Running, resp.State, "Send Running after StartPending")
		assert.Equal(t, svc.AcceptStop|svc.AcceptShutdown, resp.Accepts, "Accept stop & shutdown")
		requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
		resp = <-responses
		assert.Equal(t, svc.Running, resp.State, "Send Running after Interrogate")
		requests <- svc.ChangeRequest{Cmd: svc.Stop}
		resp = <-responses
		assert.Equal(t, svc.StopPending, resp.State, "Send StopPending after Stop")
		wg.Done()
	}()

	wg.Wait()
}

func TestHandler_HandleWindowsRequests_Cancel(t *testing.T) {
	requests := make(chan svc.ChangeRequest)
	responses := make(chan svc.Status)

	handler := &handler{}
	ctx, cancel := context.WithCancel(context.TODO())
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		handler.handleWindowsRequests(ctx, requests, responses)
		wg.Done()
	}()

	go func() {
		resp := <-responses
		assert.Equal(t, svc.StartPending, resp.State, "Send StartPending immediately")
		resp = <-responses
		assert.Equal(t, svc.Running, resp.State, "Send Running after StartPending")
		assert.Equal(t, svc.AcceptStop|svc.AcceptShutdown, resp.Accepts, "Accept stop & shutdown")
		requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
		resp = <-responses
		assert.Equal(t, svc.Running, resp.State, "Send Running after Interrogate")
		cancel()
		resp = <-responses
		assert.Equal(t, svc.StopPending, resp.State, "Send StopPending after Cancel")
		wg.Done()
	}()

	wg.Wait()
}

func TestHandler_Execute_WindowsStops(t *testing.T) {
	ctrl := gomock.NewController(t)
	taskEngine := mock_engine.NewMockTaskEngine(ctrl)
	taskEngineState := mock_dockerstate.NewMockTaskEngineState(ctrl)
	defer ctrl.Finish()
	dataClient := data.NewNoopClient()

	taskEngine.EXPECT().Disable()
	taskEngineState.EXPECT().AllTasks().Return(nil)
	taskEngineState.EXPECT().AllImageStates().Return(nil)
	taskEngineState.EXPECT().AllENIAttachments().Return(nil)
	taskEngineState.EXPECT().GetAllContainerIDs().Return([]string{"test-container"})
	taskEngineState.EXPECT().ContainerByID("test-container").Return(nil, false)

	agent := &mockAgent{}

	_, cancel := context.WithCancel(context.TODO())
	done := make(chan struct{})
	defer func() { done <- struct{}{} }()
	startFunc := func() int {
		go agent.terminationHandler(taskEngineState, dataClient, taskEngine, cancel)
		<-done // block until after the test ends so that we can test that Execute returns when Stopped
		return 0
	}
	agent.startFunc = startFunc
	handler := &handler{agent}
	requests := make(chan svc.ChangeRequest)
	responses := make(chan svc.Status)

	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		handler.Execute(nil, requests, responses)
		wg.Done()
	}()

	go func() {
		resp := <-responses
		assert.Equal(t, svc.StartPending, resp.State, "Send StartPending immediately")
		resp = <-responses
		assert.Equal(t, svc.Running, resp.State, "Send Running after StartPending")
		assert.Equal(t, svc.AcceptStop|svc.AcceptShutdown, resp.Accepts, "Accept stop & shutdown")
		time.Sleep(time.Second) // let it run for a second
		requests <- svc.ChangeRequest{Cmd: svc.Shutdown}
		resp = <-responses
		assert.Equal(t, svc.StopPending, resp.State, "Send StopPending after Shutdown")
		wg.Done()
	}()

	wg.Wait()
}

func TestHandler_Execute_AgentStops(t *testing.T) {
	agent := &mockAgent{}

	ctx, cancel := context.WithCancel(context.TODO())
	startFunc := func() int {
		<-ctx.Done()
		return 0
	}
	agent.startFunc = startFunc
	handler := &handler{agent}
	requests := make(chan svc.ChangeRequest)
	responses := make(chan svc.Status)

	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		handler.Execute(nil, requests, responses)
		wg.Done()
	}()

	go func() {
		resp := <-responses
		assert.Equal(t, svc.StartPending, resp.State, "Send StartPending immediately")
		resp = <-responses
		assert.Equal(t, svc.Running, resp.State, "Send Running after StartPending")
		assert.Equal(t, svc.AcceptStop|svc.AcceptShutdown, resp.Accepts, "Accept stop & shutdown")
		time.Sleep(time.Second) // let it run for a second
		cancel()
		resp = <-responses
		assert.Equal(t, svc.StopPending, resp.State, "Send StopPending after agent goroutine stops")
		wg.Done()
	}()

	wg.Wait()
}

func TestDoStartTaskLimitsFail(t *testing.T) {
	ctrl, credentialsManager, state, imageManager, client,
		dockerClient, stateManagerFactory, saveableOptionFactory, execCmdMgr := setup(t)
	defer ctrl.Finish()

	cfg := getTestConfig()
	cfg.Checkpoint = config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled}
	cfg.TaskCPUMemLimit.Value = config.ExplicitlyEnabled
	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   &cfg,
		dockerClient:          dockerClient,
		stateManagerFactory:   stateManagerFactory,
		saveableOptionFactory: saveableOptionFactory,
		ec2MetadataClient:     ec2.NewBlackholeEC2MetadataClient(),
	}

	dockerClient.EXPECT().SupportedVersions().Return(apiVersions)

	exitCode := agent.doStart(eventstream.NewEventStream("events", ctx),
		credentialsManager, state, imageManager, client, execCmdMgr)
	assert.Equal(t, exitcodes.ExitTerminal, exitCode)
}
