package server

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// DOPBatchOptions only bounds explicitly concurrent requests. Zero values do
// not opt legacy callers into workers, admission limits or execution deadlines.
type DOPBatchOptions struct {
	MaxConcurrency           int
	MaxInFlight              int
	MaxRequests              int
	ReservedConnections      *int
	Timeout                  time.Duration
	DisableParallelProvision bool
}

func (o DOPBatchOptions) normalized() (DOPBatchOptions, error) {
	for _, setting := range []struct {
		value             *int
		fallback, maximum int
	}{
		{&o.MaxConcurrency, 8, 32}, {&o.MaxInFlight, 8, 64}, {&o.MaxRequests, 2, 8},
	} {
		if *setting.value == 0 {
			*setting.value = setting.fallback
		}
		if *setting.value < 1 || *setting.value > setting.maximum {
			return o, fmt.Errorf("invalid DOP concurrency capacity")
		}
	}
	reserve := 8
	if o.ReservedConnections != nil {
		reserve = *o.ReservedConnections
	}
	if reserve < 0 {
		return o, fmt.Errorf("invalid DOP reserved connection count")
	}
	o.ReservedConnections = &reserve
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.Timeout < time.Second || o.Timeout > time.Hour {
		return o, fmt.Errorf("DOP batch timeout must be between one second and one hour")
	}
	return o, nil
}

// DOPBatchOptionsFromEnv is used only by the additive entrypoint. Native Config
// and upstream environment files remain untouched.
func DOPBatchOptionsFromEnv() (DOPBatchOptions, error) {
	var options DOPBatchOptions
	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"TOKENHUB_DOP_BATCH_MAX_CONCURRENCY", &options.MaxConcurrency},
		{"TOKENHUB_DOP_BATCH_MAX_INFLIGHT", &options.MaxInFlight},
		{"TOKENHUB_DOP_BATCH_MAX_REQUESTS", &options.MaxRequests},
	} {
		if raw, set := os.LookupEnv(setting.name); set {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				return options, fmt.Errorf("invalid %s", setting.name)
			}
			*setting.value = value
		}
	}
	if raw, set := os.LookupEnv("TOKENHUB_DOP_BATCH_RESERVED_CONNECTIONS"); set {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return options, fmt.Errorf("invalid DOP reserved connections")
		}
		options.ReservedConnections = &value
	}
	if raw, set := os.LookupEnv("TOKENHUB_DOP_BATCH_TIMEOUT_SECONDS"); set {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 3600 {
			return options, fmt.Errorf("invalid DOP batch timeout")
		}
		options.Timeout = time.Duration(value) * time.Second
	}
	if raw, set := os.LookupEnv("TOKENHUB_DOP_BATCH_PARALLEL_PROVISION"); set {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return options, fmt.Errorf("invalid DOP parallel provisioning switch")
		}
		options.DisableParallelProvision = !value
	}
	return options.normalized()
}
