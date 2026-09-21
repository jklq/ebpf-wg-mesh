package delivery

import "testing"

func TestBuildSchedulerConfigWithDefaults(t *testing.T) {
	t.Parallel()

	filled := BuildSchedulerConfig{}.WithDefaults()
	def := DefaultBuildSchedulerConfig()
	if filled != def {
		t.Fatalf("zero config with defaults = %+v, want %+v", filled, def)
	}
	if def.LeaseTTL <= 0 || def.AttemptLimit <= 0 || def.MaxConcurrentGlobal <= 0 ||
		def.MaxConcurrentPerProject <= 0 || def.BuildTimeout <= def.LeaseTTL || def.MaxQueueAge <= 0 {
		t.Fatalf("default scheduler config is not a usable floor: %+v", def)
	}

	custom := BuildSchedulerConfig{MaxConcurrentGlobal: 2}.WithDefaults()
	if custom.MaxConcurrentGlobal != 2 {
		t.Fatalf("WithDefaults overwrote MaxConcurrentGlobal: %+v", custom)
	}
	if custom.LeaseTTL != def.LeaseTTL || custom.AttemptLimit != def.AttemptLimit {
		t.Fatalf("WithDefaults did not fill the rest: %+v", custom)
	}
}
