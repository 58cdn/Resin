package service

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/node"
)

// SubmitIPCheckBatch resolves the current engine once. Its workers never
// re-enter the manager while Configure/Stop might be waiting for them.
func (s *ControlPlaneService) SubmitIPCheckBatch(ctx context.Context, requestID string, values []string, sources []string, force bool) (*ipcheck.BatchTask, bool, error) {
	if len(values) == 0 || len(values) > ipcheck.MaxBatchNodes {
		return nil, false, invalidArg("node_hashes: must contain 1 to 50 node hashes")
	}
	hashes := make([]node.Hash, len(values))
	for i, value := range values {
		hash, err := node.ParseHex(value)
		if err != nil {
			return nil, false, invalidArg("node_hashes: invalid node hash")
		}
		hashes[i] = hash
	}
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, false, err
	}
	task, reused, err := engine.SubmitBatch(ctx, requestID, hashes, sources, force)
	return task, reused, ipQualityServiceError(err)
}

func parseIPCheckTaskID(value string) (string, error) {
	if len(value) != 32 {
		return "", invalidArg("task_id: invalid format")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", invalidArg("task_id: invalid format")
	}
	return strings.ToLower(value), nil
}

func (s *ControlPlaneService) GetIPCheckBatch(ctx context.Context, value string) (*ipcheck.BatchTask, error) {
	id, err := parseIPCheckTaskID(value)
	if err != nil {
		return nil, err
	}
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, err
	}
	task, err := engine.GetBatch(ctx, id)
	return task, ipQualityServiceError(err)
}

func (s *ControlPlaneService) CancelIPCheckBatch(ctx context.Context, value string) (*ipcheck.BatchTask, error) {
	id, err := parseIPCheckTaskID(value)
	if err != nil {
		return nil, err
	}
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, err
	}
	task, err := engine.CancelBatch(ctx, id)
	return task, ipQualityServiceError(err)
}
