package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aarani/craftling-go/internal/auth"
	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
)

// AuthHandler wires the user and refresh-token repositories plus the JWT
// manager to HTTP endpoints.
type AuthHandler struct {
	users         *repository.UserRepository
	refreshTokens *repository.RefreshTokenRepository
	jwt           *auth.Manager
	refreshTTL    time.Duration
}

// NewAuthHandler constructs an AuthHandler.
func NewAuthHandler(
	users *repository.UserRepository,
	refreshTokens *repository.RefreshTokenRepository,
	jwt *auth.Manager,
	refreshTTL time.Duration,
) *AuthHandler {
	return &AuthHandler{
		users:         users,
		refreshTokens: refreshTokens,
		jwt:           jwt,
		refreshTTL:    refreshTTL,
	}
}

type credentials struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"` // access-token lifetime in seconds
}

// mint generates an access JWT plus a fresh refresh token (raw value + storage
// hash). It performs no persistence.
func (h *AuthHandler) mint(user *model.User, now time.Time) (access, rawRefresh, hashRefresh string, err error) {
	access, err = h.jwt.Generate(user.ID, user.Email, user.Role, now)
	if err != nil {
		return "", "", "", err
	}
	rawRefresh, hashRefresh, err = auth.GenerateRefreshToken()
	if err != nil {
		return "", "", "", err
	}
	return access, rawRefresh, hashRefresh, nil
}

func (h *AuthHandler) response(access, rawRefresh string) *tokenResponse {
	return &tokenResponse{
		AccessToken:  access,
		RefreshToken: rawRefresh,
		TokenType:    "Bearer",
		ExpiresIn:    int(h.jwt.TTL().Seconds()),
	}
}

// issueTokens mints and persists a new token pair for the user (used on
// register and login, where there is no prior token to rotate).
func (h *AuthHandler) issueTokens(ctx context.Context, user *model.User) (*tokenResponse, error) {
	now := time.Now()
	access, raw, hash, err := h.mint(user, now)
	if err != nil {
		return nil, err
	}
	if err := h.refreshTokens.Create(ctx, user.ID, hash, now.Add(h.refreshTTL)); err != nil {
		return nil, err
	}
	return h.response(access, raw), nil
}

// Register creates a new user and returns an access/refresh token pair.
func (h *AuthHandler) Register(c *gin.Context) {
	var req credentials
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		logger.FromContext(c).Error("hash password", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	user, err := h.users.Create(c.Request.Context(), req.Email, hash)
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
			return
		}
		logger.FromContext(c).Error("create user", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	tokens, err := h.issueTokens(c.Request.Context(), user)
	if err != nil {
		logger.FromContext(c).Error("issue tokens", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, tokens)
}

// Login verifies credentials and returns an access/refresh token pair.
func (h *AuthHandler) Login(c *gin.Context) {
	var req credentials
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.users.GetByEmail(c.Request.Context(), req.Email)
	if err != nil || !auth.CheckPassword(user.PasswordHash, req.Password) {
		// Same response whether the user is missing or the password is wrong,
		// to avoid leaking which emails are registered.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	tokens, err := h.issueTokens(c.Request.Context(), user)
	if err != nil {
		logger.FromContext(c).Error("issue tokens", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, tokens)
}

// Refresh exchanges a valid refresh token for a new token pair, rotating the
// presented token. Reuse of an already-rotated token is treated as theft and
// revokes the user's entire token family.
func (h *AuthHandler) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()

	rt, err := h.refreshTokens.GetByHash(ctx, auth.HashRefreshToken(req.RefreshToken))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}

	if rt.Revoked() {
		// The token was already rotated away — someone is replaying an old one.
		logger.FromContext(c).Warn("refresh token reuse detected", zap.String("user_id", rt.UserID))
		_ = h.refreshTokens.RevokeAllForUser(ctx, rt.UserID)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}

	now := time.Now()
	if rt.Expired(now) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "refresh token expired"})
		return
	}

	user, err := h.users.GetByID(ctx, rt.UserID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}

	access, raw, hash, err := h.mint(user, now)
	if err != nil {
		logger.FromContext(c).Error("mint tokens", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	// Rotate atomically: revoke the presented token and persist its replacement
	// in a single transaction.
	if err := h.refreshTokens.Rotate(ctx, rt.ID, user.ID, hash, now.Add(h.refreshTTL)); err != nil {
		logger.FromContext(c).Error("rotate refresh token", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, h.response(access, raw))
}

// Logout revokes the supplied refresh token. It always responds 204 so that it
// neither leaks token validity nor fails on an already-invalid token.
func (h *AuthHandler) Logout(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()

	if rt, err := h.refreshTokens.GetByHash(ctx, auth.HashRefreshToken(req.RefreshToken)); err == nil {
		_ = h.refreshTokens.Revoke(ctx, rt.ID)
	}
	c.Status(http.StatusNoContent)
}

// Me returns the currently authenticated user. Requires the Auth middleware.
func (h *AuthHandler) Me(c *gin.Context) {
	user, err := h.users.GetByID(c.Request.Context(), middleware.UserIDFromContext(c))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		logger.FromContext(c).Error("get user", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
