// Command init is the in-VM PID 1 for craftling game-server microVMs.
//
// The rootfs is mounted read-only and this binary owns everything
// writable (tmpfs at /tmp, /run, /dev/shm), so it acts as a minimal
// Linux init: bring kernel filesystems up, set up scratch tmpfses,
// read the run spec the image converter distilled from the OCI image
// config (internal/runspec), then launch the game server as a child
// and supervise it — forwarding termination signals, reaping orphans
// (PID 1's job), and powering the VM off when the workload exits.
//
// The image converter (internal/image) injects this binary at
// /.craftling/init and the kernel is booted with init=/.craftling/init.
// On non-Linux hosts the binary still builds (so `go build ./...` works
// from a dev Mac) but refuses to run — it only makes sense as a guest
// PID 1.
package main

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	logger := initLogger()
	defer func() { _ = logger.Sync() }()
	_ = zap.RedirectStdLog(logger)

	run(logger)
}

// initLogger installs and returns a console-format zap logger writing
// to stderr (which goes to the VM's kernel console). The init agent is
// a tiny in-VM PID 1, so it carries its own minimal logger setup rather
// than importing the main module's logger package.
func initLogger() *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encCfg),
		zapcore.Lock(os.Stderr),
		zap.InfoLevel,
	)
	logger := zap.New(core)
	zap.ReplaceGlobals(logger)
	return logger
}

// defaultPath is the PATH the init agent guarantees for the workload
// when neither the kernel boot env nor the image's run spec supplies
// one. The agent runs as PID 1 directly off the kernel, so its
// environment is whatever the kernel passed via init= — typically just
// HOME=/, sometimes a kernel-set PATH, often neither.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
