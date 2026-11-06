package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// validateRemark 校验备注为纯文本且不超过 500 字（U7）。
func validateRemark(remark string) bool {
	return utf8.RuneCountInString(remark) <= protocol.MaxRemarkRunes
}

// agentBriefs 返回某设备下全部 agent 的内嵌摘要（P-2 展开项，含只读 version / exec_path）。
func (s *Server) agentBriefs(ctx context.Context, deviceID string) ([]AgentBriefDTO, error) {
	agents, _, err := s.agentRepo.List(ctx, AgentListFilter{
		Query: ListQuery{DeviceID: deviceID, Limit: MaxListLimit},
	})
	if err != nil {
		return nil, err
	}
	briefs := make([]AgentBriefDTO, 0, len(agents))
	for _, agent := range agents {
		briefs = append(briefs, ToAgentBriefDTO(agent))
	}
	return briefs, nil
}

// handleListDevices 处理 E06 GET /api/v1/devices（Bearer）。
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := ParseListQuery(r)
	devices, total, err := s.deviceRepo.List(ctx, query)
	if err != nil {
		slog.Error("查询设备清单失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	items := make([]DeviceDTO, 0, len(devices))
	for _, device := range devices {
		briefs, briefErr := s.agentBriefs(ctx, device.ID)
		if briefErr != nil {
			slog.Error("查询设备内嵌 agent 失败", "device_id", device.ID, "err", briefErr)
			writeFail(w, protocol.CodeInternalError)
			return
		}
		items = append(items, ToDeviceDTO(device, briefs))
	}
	writeOK(w, DeviceListData{Items: items, Total: total})
}

// handleUpdateDeviceRemark 处理 E07 PATCH /api/v1/devices/{deviceId}/remark（Bearer）。
func (s *Server) handleUpdateDeviceRemark(w http.ResponseWriter, r *http.Request) {
	var request RemarkRequest
	if err := decodeJSON(r, &request); err != nil {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	if !validateRemark(request.Remark) {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	device, err := s.deviceRepo.UpdateRemark(r.Context(), r.PathValue("deviceId"), request.Remark)
	if errors.Is(err, ErrDeviceNotFound) {
		writeFail(w, protocol.CodeDeviceNotFound)
		return
	}
	if err != nil {
		slog.Error("更新设备备注失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	briefs, err := s.agentBriefs(r.Context(), device.ID)
	if err != nil {
		slog.Error("查询设备内嵌 agent 失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	writeOK(w, ToDeviceDTO(device, briefs))
}
