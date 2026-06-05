// Package reconciler drives game servers from their observed status toward
// their desired state, using a provisioner backend.
package reconciler

import (
	"context"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/provisioner"
	"github.com/aarani/craftling-go/internal/repository"
	"go.uber.org/zap"
)

// Reconciler periodically converges game servers toward their desired state.
type Reconciler struct {
	servers *repository.GameServerRepository
	prov    provisioner.Provisioner
	log     *zap.Logger
}

// New constructs a Reconciler.
func New(servers *repository.GameServerRepository, prov provisioner.Provisioner, log *zap.Logger) *Reconciler {
	return &Reconciler{servers: servers, prov: prov, log: log}
}

// Run reconciles on each tick until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.ReconcileOnce(ctx)
		}
	}
}

// ReconcileOnce processes one batch of servers needing reconciliation.
func (r *Reconciler) ReconcileOnce(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	servers, err := r.servers.ListReconcilable(opCtx)
	if err != nil {
		r.log.Error("list reconcilable servers", zap.Error(err))
		return
	}

	for i := range servers {
		s := &servers[i]
		if err := r.reconcile(opCtx, s); err != nil {
			r.log.Error("reconcile server", zap.String("id", s.ID), zap.Error(err))
			_ = r.servers.MarkStatus(opCtx, s.ID, model.StatusError, err.Error())
		}
	}
}

// reconcile advances a single server one step toward its desired state.
func (r *Reconciler) reconcile(ctx context.Context, s *model.GameServer) error {
	switch s.DesiredState {
	case model.DesiredDeleted:
		return r.delete(ctx, s)
	case model.DesiredStopped:
		return r.stop(ctx, s)
	case model.DesiredRunning:
		return r.start(ctx, s)
	default:
		return nil
	}
}

func (r *Reconciler) start(ctx context.Context, s *model.GameServer) error {
	if s.Status == model.StatusRunning {
		return nil
	}
	if err := r.servers.MarkStatus(ctx, s.ID, model.StatusProvisioning, ""); err != nil {
		return err
	}
	inst, err := r.prov.Provision(ctx, s)
	if err != nil {
		return err
	}
	r.log.Info("server provisioned",
		zap.String("id", s.ID), zap.String("vm_id", inst.VMID))
	return r.servers.MarkRunning(ctx, s.ID, inst.VMID, inst.Host, inst.Port)
}

func (r *Reconciler) stop(ctx context.Context, s *model.GameServer) error {
	if s.Status == model.StatusStopped {
		return nil
	}
	if err := r.servers.MarkStatus(ctx, s.ID, model.StatusStopping, ""); err != nil {
		return err
	}
	if err := r.prov.Deprovision(ctx, s); err != nil {
		return err
	}
	r.log.Info("server stopped", zap.String("id", s.ID))
	return r.servers.MarkStopped(ctx, s.ID)
}

func (r *Reconciler) delete(ctx context.Context, s *model.GameServer) error {
	if s.Status != model.StatusDeleting {
		if err := r.servers.MarkStatus(ctx, s.ID, model.StatusDeleting, ""); err != nil {
			return err
		}
	}
	if err := r.prov.Deprovision(ctx, s); err != nil {
		return err
	}
	r.log.Info("server deleted", zap.String("id", s.ID))
	return r.servers.SoftDelete(ctx, s.ID)
}
