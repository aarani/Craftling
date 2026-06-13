package handler

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"

	"github.com/aarani/craftling-go/internal/image"
	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/scheduler"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Default resource allocation for a new server.
const (
	defaultCPUs     = 2
	defaultMemoryMB = 2048
)

// ServerHandler serves the game-server CRUD endpoints.
type ServerHandler struct {
	servers *repository.GameServerRepository
	sched   *scheduler.Scheduler
}

// NewServerHandler constructs a ServerHandler.
func NewServerHandler(servers *repository.GameServerRepository, sched *scheduler.Scheduler) *ServerHandler {
	return &ServerHandler{servers: servers, sched: sched}
}

type createServerRequest struct {
	Name    string `json:"name" binding:"required,min=1,max=64"`
	Version string `json:"version" binding:"required"`
	// Image is an optional OCI/docker reference. When set, the server
	// boots a squashfs rootfs built from it (the agent's OCI path); the
	// digest is resolved and pinned here so the rootfs is reproducible.
	// Empty selects the legacy per-version ext4 image.
	Image    string `json:"image" binding:"omitempty"`
	// Env is the per-server environment, typically a marketplace template's
	// resolved answers (e.g. {"EULA":"TRUE","MODE":"survival"}). It is stored
	// as sorted "KEY=VALUE" entries and merged over the image's OCI env by the
	// agent. Validated manually (see envEntries) rather than via binding tags.
	Env      map[string]string `json:"env" binding:"omitempty"`
	CPUs     int               `json:"cpus" binding:"omitempty,min=1,max=16"`
	MemoryMB int               `json:"memory_mb" binding:"omitempty,min=512,max=65536"`
}

type updateServerRequest struct {
	Name         *string `json:"name" binding:"omitempty,min=1,max=64"`
	Version      *string `json:"version" binding:"omitempty,min=1"`
	DesiredState *string `json:"desired_state" binding:"omitempty,oneof=running stopped"`
}

// Create provisions a new game server (desired state: running).
func (h *ServerHandler) Create(c *gin.Context) {
	var req createServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cpus := orDefault(req.CPUs, defaultCPUs)
	memoryMB := orDefault(req.MemoryMB, defaultMemoryMB)

	env, err := envEntries(req.Env)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Reject a spec no host could ever run, rather than admitting a server that
	// would sit unschedulable forever. With no hosts yet, creation is allowed —
	// the server waits for a host to join.
	if ok, err := h.sched.CanEverFit(c.Request.Context(), cpus, memoryMB); err != nil {
		logger.FromContext(c).Error("capacity check", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	} else if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "requested resources exceed the capacity of any host in the fleet"})
		return
	}

	// Pin the image to an exact digest at create time so the rootfs the
	// agent builds is reproducible across reschedules and restarts. A bad
	// reference or an unreachable registry fails creation rather than
	// admitting a server that can never boot.
	var imageRef, imageDigest string
	if req.Image != "" {
		digest, err := image.ResolveDigest(c.Request.Context(), req.Image)
		if err != nil {
			logger.FromContext(c).Warn("resolve image digest", zap.String("image", req.Image), zap.Error(err))
			c.JSON(http.StatusBadRequest, gin.H{"error": "could not resolve image reference"})
			return
		}
		imageRef, imageDigest = req.Image, digest
	}

	s := &model.GameServer{
		OwnerID:      middleware.UserIDFromContext(c),
		Name:         req.Name,
		Game:         model.GameMinecraft,
		Version:      req.Version,
		Image:        imageRef,
		ImageDigest:  imageDigest,
		Env:          env,
		CPUs:         cpus,
		MemoryMB:     memoryMB,
		DesiredState: model.DesiredRunning,
		Status:       model.StatusPending,
	}
	if err := h.servers.Create(c.Request.Context(), s); err != nil {
		logger.FromContext(c).Error("create server", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, s)
}

// List returns the authenticated user's servers.
func (h *ServerHandler) List(c *gin.Context) {
	servers, err := h.servers.ListByOwner(c.Request.Context(), middleware.UserIDFromContext(c))
	if err != nil {
		logger.FromContext(c).Error("list servers", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// Get returns a single owned server.
func (h *ServerHandler) Get(c *gin.Context) {
	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s)
}

// Update edits the spec and/or desired state of an owned server.
func (h *ServerHandler) Update(c *gin.Context) {
	var req updateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	if req.Name != nil || req.Version != nil {
		name, version := s.Name, s.Version
		if req.Name != nil {
			name = *req.Name
		}
		if req.Version != nil {
			version = *req.Version
		}
		if err := h.servers.UpdateSpec(ctx, s.ID, name, version); err != nil {
			logger.FromContext(c).Error("update server spec", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
	}

	if req.DesiredState != nil {
		if err := h.servers.SetDesiredState(ctx, s.ID, *req.DesiredState); err != nil {
			logger.FromContext(c).Error("set desired state", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
	}

	updated, err := h.servers.GetByID(ctx, s.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete marks an owned server for teardown; the reconciler removes it.
func (h *ServerHandler) Delete(c *gin.Context) {
	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	if err := h.servers.SetDesiredState(c.Request.Context(), s.ID, model.DesiredDeleted); err != nil {
		logger.FromContext(c).Error("mark server deleted", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "deleting"})
}

// RequestBackup flags an owned server for an on-demand world snapshot. It only
// records intent; the reconciler (the sole writer of compute side effects) takes
// the snapshot via the agent on its next tick.
func (h *ServerHandler) RequestBackup(c *gin.Context) {
	s, ok := h.ownedOr404(c)
	if !ok {
		return
	}
	if err := h.servers.RequestBackup(c.Request.Context(), s.ID); err != nil {
		logger.FromContext(c).Error("request backup", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "backup requested"})
}

// ownedOr404 loads the server in the URL and verifies the caller owns it.
// It writes a 404 for both missing and non-owned servers (no existence leak).
func (h *ServerHandler) ownedOr404(c *gin.Context) (*model.GameServer, bool) {
	s, err := h.servers.GetByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			logger.FromContext(c).Error("get server", zap.Error(err))
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return nil, false
	}
	if s.OwnerID != middleware.UserIDFromContext(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return nil, false
	}
	return s, true
}

func orDefault(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

// envVarName matches a POSIX-ish environment variable name: a letter or
// underscore followed by letters, digits, or underscores.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	maxEnvVars     = 100
	maxEnvValueLen = 8192
)

// envEntries validates a per-server env map and flattens it into sorted
// "KEY=VALUE" entries. Sorting makes the stored spec deterministic regardless
// of JSON object order. Returns nil for an empty map so the server keeps the
// image's stock environment.
func envEntries(m map[string]string) ([]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	if len(m) > maxEnvVars {
		return nil, fmt.Errorf("too many env vars: %d (max %d)", len(m), maxEnvVars)
	}
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if !envVarName.MatchString(k) {
			return nil, fmt.Errorf("invalid env var name %q", k)
		}
		if len(v) > maxEnvValueLen {
			return nil, fmt.Errorf("env var %q value too long (max %d bytes)", k, maxEnvValueLen)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + m[k]
	}
	return out, nil
}
