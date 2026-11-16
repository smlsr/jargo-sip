package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/smlsr/jargo-sip/transport/sip"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("receptionist", "err", err)
		os.Exit(1)
	}
}

func run() error {
	loadEnvFiles()
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return errors.New("GEMINI_API_KEY is empty")
	}
	cfg, err := listenConfigFromEnv()
	if err != nil {
		return err
	}
	speech := newGemini(key, fallbackDialogModel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if names, err := speech.listModels(ctx); err != nil {
		slog.Warn("model list failed, using the recorded Flash model", "model", speech.model, "err", err)
	} else if picked, err := selectDialogModel(names); err != nil {
		slog.Warn("model list had no text Flash model, using the recorded one", "model", speech.model, "err", err)
	} else {
		speech.model = picked
	}
	slog.Info("speech",
		"dialog", speech.model,
		"recognize", speech.model,
		"tts", ttsModel,
		"voice", speech.voice,
	)

	ep, err := sip.New(cfg.endpoint())
	if err != nil {
		return err
	}
	if err := ep.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = ep.Close() }()
	slog.Info("listening",
		"port", cfg.Port,
		"domain", cfg.Domain,
		"user", cfg.User,
		"allow", cfg.AllowFrom,
		"audio", "8 kHz G.711",
	)
	return serve(ctx, ep, speech)
}
