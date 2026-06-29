package core

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"
)

// CreateCapsule creates a new capsule in the specified cluster.
func (c *Core) CreateCapsule(ctx context.Context, req CreateCapsuleRequest) (*CapsuleResource, error) {
	if req.Cluster == "" {
		return nil, WrapInvalidArgument("cluster is required")
	}
	if req.Name == "" {
		return nil, WrapInvalidArgument("name is required")
	}
	if req.Image == "" {
		return nil, WrapInvalidArgument("image is required")
	}
	if req.Orbit == "" {
		return nil, WrapInvalidArgument("orbit is required")
	}

	c.logger.Info("api: creating capsule",
		zap.String("name", req.Name),
		zap.String("cluster", req.Cluster),
		zap.String("image", req.Image))

	result, err := c.node.CapsuleCreate(ctx, req.Cluster, req)
	if err != nil {
		// Preserve sentinel mapping: known classes (already-exists,
		// invalid-argument) stay typed so the transport layer surfaces
		// the correct gRPC code instead of collapsing to Internal.
		if errors.Is(err, ErrAlreadyExists) ||
			errors.Is(err, ErrInvalidArgument) ||
			errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	return result, nil
}

// GetCapsule returns a single capsule by ID.
func (c *Core) GetCapsule(ctx context.Context, req GetCapsuleRequest) (*CapsuleResource, error) {
	if req.ID == "" {
		return nil, WrapInvalidArgument("capsule ID is required")
	}

	result, err := c.node.CapsuleGet(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, WrapNotFound("capsule " + req.ID)
	}
	return result, nil
}

// ListCapsules returns capsules matching the filter criteria.
func (c *Core) ListCapsules(ctx context.Context, req ListCapsulesRequest) (*ListCapsulesResponse, error) {
	return c.node.CapsuleList(ctx, req)
}

// DeleteCapsule removes a capsule by ID.
func (c *Core) DeleteCapsule(ctx context.Context, req DeleteCapsuleRequest) error {
	if req.ID == "" {
		return WrapInvalidArgument("capsule ID is required")
	}

	c.logger.Info("api: deleting capsule", zap.String("id", req.ID))
	return c.node.CapsuleDelete(ctx, req.ID)
}

// UpdateCapsule updates an existing capsule's spec.
func (c *Core) UpdateCapsule(ctx context.Context, req UpdateCapsuleRequest) (*CapsuleResource, error) {
	if req.ID == "" {
		return nil, WrapInvalidArgument("capsule ID is required")
	}

	c.logger.Info("api: updating capsule",
		zap.String("id", req.ID),
		zap.String("image", req.Image))

	result, err := c.node.CapsuleUpdate(ctx, req)
	if err != nil {
		return nil, err
	}
	return result, nil
}
