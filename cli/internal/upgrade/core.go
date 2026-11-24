package upgrade

import (
	"context"
	"errors"
	"os"
	"time"

	"knot-cli/internal/coreclient"
)

var errOtherCore = errors.New("running Core is from a different installation")

type processWatch struct {
	exited func() (bool, error)
	close  func()
}

type runningCore struct {
	runtime coreclient.Runtime
	watch   processWatch
}

func probeCore(ctx context.Context, client *coreclient.Client) (coreclient.Runtime, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, err := client.Runtime(ctx)
	if coreclient.Code(err) == "UNREACHABLE" {
		return coreclient.Runtime{}, nil
	}
	if err != nil {
		return coreclient.Runtime{}, err
	}
	if info.InstanceID == "" || info.PID <= 0 || info.APIVersion != "v1" {
		return coreclient.Runtime{}, coreclient.Failure("PROTOCOL_ERROR")
	}
	return info, nil
}

func inspectCore(info coreclient.Runtime, executable string) (*runningCore, error) {
	if info.InstanceID == "" {
		return nil, nil
	}
	watch, image, err := watchProcess(info.PID)
	if err != nil {
		return nil, err
	}
	want, wantErr := os.Stat(executable)
	actual, actualErr := os.Stat(image)
	if wantErr != nil || actualErr != nil || !os.SameFile(want, actual) {
		watch.close()
		return nil, errOtherCore
	}
	return &runningCore{runtime: info, watch: watch}, nil
}

func (core *runningCore) wait(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		exited, err := core.watch.exited()
		if exited || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
