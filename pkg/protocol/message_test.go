package protocol

import (
	"encoding/json"
	"testing"
)

// TestFrameTypesComplete 校验 F01~F15 帧常量完整、有序、唯一，且不存在 Multica 团队功能帧（C-14）。
func TestFrameTypesComplete(t *testing.T) {
	expected := []string{
		FrameDeviceRegister,    // F01
		FrameDeviceRegisterAck, // F02
		FrameDeviceHeartbeat,   // F03
		FrameHeartbeatAck,      // F04
		FrameChatSend,          // F05
		FrameChatSendAck,       // F06
		FrameChatChunk,         // F07
		FrameChatDone,          // F08
		FrameChatError,         // F09
		FrameChatStop,          // F10
		FrameAgentStatus,       // F11
		FrameAgentUnregistered, // F12
		FrameAgentRegistered,   // F13
		FrameClientReady,       // F14
		FrameDeviceStatus,      // F15
	}
	types := AllFrameTypes()
	if len(types) != 15 {
		t.Fatalf("帧类型数量应为 15，实际 %d", len(types))
	}
	seen := make(map[string]struct{}, len(types))
	for index, frameType := range expected {
		if types[index] != frameType {
			t.Errorf("F%02d 期望 %q，实际 %q", index+1, frameType, types[index])
		}
		if _, exists := seen[frameType]; exists {
			t.Errorf("帧名重复: %s", frameType)
		}
		seen[frameType] = struct{}{}
		if !IsValidFrameType(frameType) {
			t.Errorf("合法帧名未被识别: %s", frameType)
		}
	}
	for _, forbidden := range []string{"task.claim", "issue.update", "workspace.list", ""} {
		if IsValidFrameType(forbidden) {
			t.Errorf("不应存在帧类型: %q", forbidden)
		}
	}
	// 返回值是副本，外部修改不得污染全局表。
	types[0] = "mutated"
	if AllFrameTypes()[0] != FrameDeviceRegister {
		t.Error("AllFrameTypes 必须返回副本")
	}
}

// TestMessageEncodeDecodeRoundTrip 校验信封 Encode/Decode 与 payload 反序列化往返无损。
func TestMessageEncodeDecodeRoundTrip(t *testing.T) {
	payload := ChatChunkPayload{
		AgentID:      "dev_9f2a1c:claude",
		SessionID:    "ses_7b31",
		AssistantSeq: 42,
		ChunkIndex:   1,
		Delta:        "检查一下……",
	}
	message, err := NewMessage(FrameChatChunk, 12, payload)
	if err != nil {
		t.Fatalf("NewMessage 失败: %v", err)
	}
	encoded := message.Encode()
	if len(encoded) == 0 {
		t.Fatal("Encode 返回空字节")
	}
	if _, err := message.Marshal(); err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}

	var decoded Message
	if err := decoded.Decode(encoded); err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	if decoded.Type != FrameChatChunk {
		t.Errorf("type 期望 %s，实际 %s", FrameChatChunk, decoded.Type)
	}
	if decoded.Seq != 12 {
		t.Errorf("seq 期望 12，实际 %d", decoded.Seq)
	}
	var gotPayload ChatChunkPayload
	if err := decoded.DecodePayload(&gotPayload); err != nil {
		t.Fatalf("DecodePayload 失败: %v", err)
	}
	if gotPayload != payload {
		t.Errorf("payload 往返不一致: got=%+v want=%+v", gotPayload, payload)
	}

	// 信封字段必须是 snake_case 且与前端 types.ts 一致：{type, seq, payload}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("解析信封 JSON 失败: %v", err)
	}
	for _, key := range []string{"type", "seq", "payload"} {
		if _, ok := envelope[key]; !ok {
			t.Errorf("信封缺少字段 %s", key)
		}
	}
}

// TestMessageDecodeValidation 校验非法帧的容错行为。
func TestMessageDecodeValidation(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{name: "空内容", data: nil},
		{name: "非法 JSON", data: []byte("{not-json")},
		{name: "缺少 type", data: []byte(`{"seq":1,"payload":{}}`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var message Message
			if err := message.Decode(testCase.data); err == nil {
				t.Error("期望返回错误，实际为 nil")
			}
		})
	}
	var nilMessage *Message
	readyFrame := []byte(`{"type":"` + FrameClientReady + `"}`)
	if err := nilMessage.Decode(readyFrame); err == nil {
		t.Error("nil 接收者应返回错误")
	}
	empty, err := NewMessage(FrameClientReady, 1, nil)
	if err != nil {
		t.Fatalf("NewMessage(nil payload) 失败: %v", err)
	}
	if string(empty.Payload) != "{}" {
		t.Errorf("nil payload 应下发 {}，实际 %s", empty.Payload)
	}
	if _, err := NewMessage("unknown.frame", 1, nil); err == nil {
		t.Error("未知帧类型应被拒绝")
	}
}

// TestErrorCodes 校验 §6.5 错误码表完整、文案与 HTTP 状态映射正确。
func TestErrorCodes(t *testing.T) {
	codes := AllCodes()
	if len(codes) != 19 {
		t.Fatalf("错误码数量应为 19，实际 %d", len(codes))
	}
	expectedStatus := map[int]int{
		CodeOK:                      200,
		CodeUnauthorized:            401,
		CodeInvalidPassword:         401,
		CodeTokenExpired:            401,
		CodeBadRequest:              400,
		CodeNotFound:                404,
		CodeDeviceNotFound:          404,
		CodeAgentNotFound:           404,
		CodeAgentNotRegistered:      409,
		CodeAgentOffline:            409,
		CodeSessionNotFound:         404,
		CodeMessageTooLarge:         413,
		CodeGenerationInProgress:    409,
		CodeInternalError:           500,
		CodeAdapterUnauthorized:     401,
		CodeProtocolVersionMismatch: 426,
		CodeCLIStartFailed:          502,
		CodeResumeRejected:          409,
		CodeGenerationTimeout:       504,
	}
	for _, code := range codes {
		if !IsKnownCode(code) {
			t.Errorf("错误码 %d 未登记", code)
		}
		if ErrorMessage(code) == "" {
			t.Errorf("错误码 %d 缺少中文文案", code)
		}
		if HTTPStatus(code) != expectedStatus[code] {
			t.Errorf("错误码 %d 的 HTTP 状态期望 %d，实际 %d", code, expectedStatus[code], HTTPStatus(code))
		}
	}
	if ErrorMessage(9999) != ErrorMessage(CodeInternalError) {
		t.Error("未知错误码应回退为内部错误文案")
	}
	if HTTPStatus(9999) != 500 {
		t.Error("未知错误码应回退为 HTTP 500")
	}
}

// TestAgentTypes 校验 9 种 Agent 描述符与 4 个协议族（D-1 / §8.3）。
func TestAgentTypes(t *testing.T) {
	types := AgentTypes()
	if len(types) != 9 {
		t.Fatalf("Agent 类型数量应为 9，实际 %d", len(types))
	}
	validFamilies := map[string]bool{
		FamilyStream: true, FamilyACP: true, FamilyAppServer: true, FamilyJSON: true,
	}
	seen := make(map[string]struct{}, len(types))
	for _, item := range types {
		if !IsValidAgentType(item.Type) {
			t.Errorf("未识别 Agent 类型: %s", item.Type)
		}
		if !validFamilies[item.Family] {
			t.Errorf("Agent %s 的协议族非法: %s", item.Type, item.Family)
		}
		if item.DisplayName == "" {
			t.Errorf("Agent %s 缺少 display_name", item.Type)
		}
		if _, ok := seen[item.Type]; ok {
			t.Errorf("Agent 类型重复: %s", item.Type)
		}
		seen[item.Type] = struct{}{}
	}
	if IsValidAgentType("unknown-agent") {
		t.Error("未知 Agent 类型不应通过")
	}
}
