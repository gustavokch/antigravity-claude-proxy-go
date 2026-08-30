package headroom

import "context"

type Pipeline struct {
	stages []Stage
}

func NewPipeline(stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages}
}

func (p *Pipeline) Run(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if cfg == nil || (!cfg.Enabled && !cfg.CommandCrusher) {
		return nil
	}
	for _, stage := range p.stages {
		if !cfg.Enabled && stage.Name() != "command_crusher" {
			continue
		}
		if err := stage.Execute(ctx, reqCtx, cfg); err != nil {
			return err
		}
	}
	return nil
}
