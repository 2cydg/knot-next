package coreclient

import "context"

// Load brackets public API reads with instance checks, without local discovery.
func Load(ctx context.Context, url, secret string) (s Snapshot, err error) {
	c, err := New(url, secret)
	if err != nil {
		return s, err
	}
	s.Client = c
	ok := false
	defer func() {
		if !ok {
			c.Close()
			s = Snapshot{}
		}
	}()
	s.Runtime, err = c.Runtime(ctx)
	if err != nil {
		return s, err
	}
	if s.Runtime.InstanceID == "" {
		return s, Failure("PROTOCOL_ERROR")
	}
	if s.Runtime.APIVersion != "v1" {
		return s, Failure("API_UNSUPPORTED")
	}
	s.Version, err = c.Version(ctx)
	if err != nil {
		return s, err
	}
	if s.Version.Version == "" || s.Version.APIVersion != "v1" {
		return s, Failure("API_UNSUPPORTED")
	}
	s.Health, err = c.Health(ctx)
	if err != nil {
		return s, err
	}
	if !s.Health.Authenticated {
		return s, Failure("UNAUTHORIZED")
	}
	s.Capabilities, err = c.Capabilities(ctx)
	if err != nil {
		return s, err
	}
	if s.Available("config") {
		s.Config, err = c.Summary(ctx)
		if err != nil {
			return s, err
		}
	}
	end, err := c.Runtime(ctx)
	if err != nil {
		return s, err
	}
	if end.InstanceID != s.Runtime.InstanceID {
		return s, Failure("INSTANCE_CHANGED")
	}
	ok = true
	return s, nil
}
