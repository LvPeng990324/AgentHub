package adapter

import (
	"errors"
	"testing"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// TestBuildResumeAllKinds 校验统一恢复入口的 5 种写法 + 空 sessionID 新建计划。
func TestBuildResumeAllKinds(t *testing.T) {
	registry := NewRegistry()
	const sessionID = "ses-from-platform"

	cases := []struct {
		agentType  string
		wantArgs   []string
		wantMethod string
		wantParam  string
	}{
		{protocol.AgentTypeClaude, []string{"--resume", sessionID}, "", ""},
		{protocol.AgentTypeQwen, []string{"--resume", sessionID}, "", ""},
		{protocol.AgentTypeCursor, []string{"--resume", sessionID}, "", ""},
		{protocol.AgentTypeHermes, nil, protocol.ResumeKindSessionResume, "sessionId"},
		{protocol.AgentTypeKimi, nil, protocol.ResumeKindSessionResume, "sessionId"},
		{protocol.AgentTypeCodex, nil, protocol.ResumeKindThreadResume, "threadId"},
		{protocol.AgentTypeOpencode, []string{"--session", sessionID}, "", ""},
		{protocol.AgentTypeOpenclaw, []string{"--session-id", sessionID}, "", ""},
	}
	for _, testCase := range cases {
		descriptor, ok := registry.Get(testCase.agentType)
		if !ok {
			t.Fatalf("缺少描述符 %s", testCase.agentType)
		}
		plan := BuildResume(descriptor, sessionID)
		if testCase.wantMethod != "" {
			if plan.Method != testCase.wantMethod {
				t.Errorf("%s method 期望 %s，实际 %s", testCase.agentType, testCase.wantMethod, plan.Method)
			}
			if plan.Params[testCase.wantParam] != sessionID {
				t.Errorf("%s 缺少参数 %s: %+v", testCase.agentType, testCase.wantParam, plan.Params)
			}
			if len(plan.Args) != 0 {
				t.Errorf("%s RPC 恢复不应追加 argv: %v", testCase.agentType, plan.Args)
			}
		} else {
			if len(plan.Args) != 2 || plan.Args[0] != testCase.wantArgs[0] || plan.Args[1] != sessionID {
				t.Errorf("%s argv 期望 %v，实际 %v", testCase.agentType, testCase.wantArgs, plan.Args)
			}
		}
	}

	// session/load 是 P2 预留枚举，本期 9 种未用，但 BuildResume 必须支持。
	loadDescriptor := Descriptors[protocol.AgentTypeHermes]
	loadDescriptor.Resume = ResumeLoadRPC
	loadPlan := BuildResume(loadDescriptor, sessionID)
	if loadPlan.Method != protocol.ResumeKindSessionLoad || loadPlan.Params["sessionId"] != sessionID {
		t.Errorf("session/load 写法错误: %+v", loadPlan)
	}
}

// TestBuildResumeEmptySessionID 校验空恢复标识 → 新建计划（不追加 argv、不伪造恢复 id）。
func TestBuildResumeEmptySessionID(t *testing.T) {
	registry := NewRegistry()
	for _, agentType := range registry.Types() {
		descriptor, _ := registry.Get(agentType)
		plan := BuildResume(descriptor, "")
		if len(plan.Args) != 0 {
			t.Errorf("%s 新会话不应追加恢复 argv: %v", agentType, plan.Args)
		}
		switch descriptor.Family {
		case FamilyACP:
			if plan.Method != rpcSessionNew {
				t.Errorf("%s 新会话应走 session/new，实际 %s", agentType, plan.Method)
			}
		case FamilyAppServer:
			if plan.Method != rpcThreadStart {
				t.Errorf("%s 新会话应走 thread/start，实际 %s", agentType, plan.Method)
			}
		default:
			if plan.Method != "" {
				t.Errorf("%s argv 族不应有 RPC method，实际 %s", agentType, plan.Method)
			}
		}
	}
}

// TestClassifyResumeFailure 校验拒绝检测与「恢复拒绝不可区分」类型的一律降级。
func TestClassifyResumeFailure(t *testing.T) {
	registry := NewRegistry()
	claude, _ := registry.Get(protocol.AgentTypeClaude)
	hermes, _ := registry.Get(protocol.AgentTypeHermes)
	opencode, _ := registry.Get(protocol.AgentTypeOpencode)
	codex, _ := registry.Get(protocol.AgentTypeCodex)

	if ClassifyResumeFailure(nil, claude) {
		t.Error("nil error 不应判定为恢复被拒")
	}
	for _, message := range []string{
		"Invalid session identifier",
		"session not found",
		"unknown session id",
		"failed to resume session: expired",
		"No such session",
	} {
		if !ClassifyResumeFailure(errors.New(message), hermes) {
			t.Errorf("应识别恢复拒绝: %q", message)
		}
	}
	if ClassifyResumeFailure(errors.New("network timeout"), claude) {
		t.Error("普通错误不应误判为恢复被拒")
	}
	// opencode / codex 的恢复拒绝不可区分：任何失败一律降级新会话（U4 / R-18）。
	for _, descriptor := range []Descriptor{opencode, codex} {
		if !ClassifyResumeFailure(errors.New("some opaque failure"), descriptor) {
			t.Errorf("%s 属于恢复拒绝不可区分类型，任何失败都应降级", descriptor.Type)
		}
	}
	if !IsResumeRejected("Invalid session identifier", claude) {
		t.Error("IsResumeRejected 应与 ClassifyResumeFailure 一致")
	}
	plan := NewSessionPlan(codex)
	if plan.Method != rpcThreadStart || len(plan.Args) != 0 {
		t.Errorf("降级计划应为 thread/start 且无 argv: %+v", plan)
	}
}
