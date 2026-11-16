package main

import (
	"context"
	"log/slog"

	"github.com/gojargo/jargo/frames"
	"github.com/gojargo/jargo/pipeline"
	"github.com/gojargo/jargo/processor/aggregators"
	"github.com/gojargo/jargo/processor/turns"
	"github.com/gojargo/jargo/processor/vadproc"
	"github.com/gojargo/jargo/provider/google/gemini"
	"github.com/gojargo/jargo/service/llm"
	"github.com/gojargo/jargo/transport"
	"github.com/gojargo/jargo/utils/events"
	"github.com/gojargo/jargo/workers"
	"github.com/smlsr/jargo-sip/transport/sip"
)

// runCall places one established call on a jargo pipeline: listen, reply,
// and keep talking until the caller is done or the call ends.
func runCall(parent context.Context, call *sip.Call, speech *geminiClient) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-call.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	params := transport.DefaultParams()
	params.AudioInSampleRate = pipelineInRate
	params.AudioOutSampleRate = pipelineOutRate
	t := sip.NewTransport(call, params)

	var worker *pipeline.Worker
	convo := frames.NewLLMContext("")
	convo.SetTools([]frames.Tool{{
		Name:        endCallTool,
		Description: endCallToolDescription,
		Parameters:  []byte(`{"type":"object","properties":{}}`),
		Handler: llm.FunctionCallHandler(func(ctx context.Context, p llm.FunctionCallParams) error {
			no := false
			if err := p.Result(ctx, "The call is ending after the goodbye.", &frames.FunctionCallResultProperties{
				RunLLM: &no,
			}); err != nil {
				return err
			}
			if worker != nil {
				worker.QueueFrame(frames.NewEndFrame())
			}
			return nil
		}),
	}})

	model := gemini.NewLLM(gemini.Config{
		APIKey:    speech.key,
		Model:     speech.model,
		MaxTokens: 160,
	})
	model.SetSystemInstruction(systemPrompt)

	agg := aggregators.New(convo, aggregators.WithTurns(turns.Config{
		Strategies: turns.UserTurnStrategies{
			Start: turns.DefaultStartStrategies(),
			Stop:  []turns.StopStrategy{turns.NewSpeechTimeoutStop(turns.SpeechTimeoutConfig{})},
		},
	}))

	pipe := pipeline.New(
		t.Input(),
		vadproc.New(vadproc.Config{VAD: newEnergyVAD()}),
		speech.newSTT(),
		agg.User(),
		model,
		speech.newTTS(),
		t.Output(),
		agg.Assistant(),
	)
	worker = pipeline.NewWorker(pipe, pipeline.WorkerConfig{
		Params: pipeline.Params{
			AudioInSampleRate:  pipelineInRate,
			AudioOutSampleRate: pipelineOutRate,
			EnableMetrics:      true,
			EnableUsageMetrics: true,
		},
		ProcessorUnusablePolicy: pipeline.UnusableEnd,
	})

	events.On(t.Events(), transport.EventClientConnected, func(context.Context, any) {
		slog.Info("caller connected")
		convo.AddMessage(frames.Message{Role: frames.RoleDeveloper, Text: greetCue})
		worker.QueueFrames([]frames.Frame{frames.NewLLMRunFrame()})
	})
	events.On(t.Events(), transport.EventClientDisconnected, func(context.Context, any) {
		slog.Info("caller disconnected")
		cancel()
	})

	off := false
	r := workers.NewRunner(workers.RunnerConfig{HandleInterrupt: &off})
	r.AddWorkers(ctx, worker)
	return r.Run(ctx, workers.RunOptions{})
}
