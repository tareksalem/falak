package core

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// JoinCluster joins the node to a cluster.
func (c *Core) JoinCluster(ctx context.Context, req JoinClusterRequest) error {
	if req.Path == "" {
		return WrapInvalidArgument("cluster path is required")
	}
	if req.PSK == "" {
		return WrapInvalidArgument("PSK is required")
	}
	c.logger.Info("api: joining cluster", zap.String("path", req.Path))
	return c.node.ClusterJoin(ctx, req)
}

// LeaveCluster leaves a cluster.
func (c *Core) LeaveCluster(ctx context.Context, path string) error {
	if path == "" {
		return WrapInvalidArgument("cluster path is required")
	}
	c.logger.Info("api: leaving cluster", zap.String("path", path))
	return c.node.ClusterLeave(ctx, path)
}

// ListClusters returns all joined clusters.
func (c *Core) ListClusters(ctx context.Context) (*ListClustersResponse, error) {
	return c.node.ClusterList(ctx)
}

// ListNodes returns nodes in a cluster.
func (c *Core) ListNodes(ctx context.Context, req ListNodesRequest) (*ListNodesResponse, error) {
	if req.Cluster == "" {
		return nil, fmt.Errorf("%w: cluster is required", ErrInvalidArgument)
	}
	return c.node.NodeList(ctx, req)
}

// GetNode returns a single node.
func (c *Core) GetNode(ctx context.Context, cluster, nodeID string) (*NodeResource, error) {
	if nodeID == "" {
		return nil, WrapInvalidArgument("node ID is required")
	}
	return c.node.NodeGet(ctx, cluster, nodeID)
}
