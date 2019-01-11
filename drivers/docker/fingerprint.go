package docker

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

// fingerprinted returns whether the driver has fingerprinted before
func (d *Driver) fingerprinted() bool {
	d.fingerprintLock.Lock()
	defer d.fingerprintLock.Unlock()
	return d.hasFingerprinted
}

// setFingerprinted marks the driver as having fingerprinted once before
func (d *Driver) setFingerprinted() {
	d.fingerprintLock.Lock()
	d.hasFingerprinted = true
	d.fingerprintLock.Unlock()
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	defer func() {
		d.setFingerprinted()
	}()

	fp := &drivers.Fingerprint{
		Attributes:        map[string]*pstructs.Attribute{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}
	client, _, err := d.dockerClients()
	if err != nil {
		if !d.fingerprinted() {
			d.logger.Info("failed to initialize client", "error", err)
		}
		return &drivers.Fingerprint{
			Health:            drivers.HealthStateUndetected,
			HealthDescription: "Failed to initialize docker client",
		}
	}

	env, err := client.Version()
	if err != nil {
		if !d.fingerprinted() {
			d.logger.Debug("could not connect to docker daemon", "endpoint", client.Endpoint(), "error", err)
		}
		return &drivers.Fingerprint{
			Health:            drivers.HealthStateUnhealthy,
			HealthDescription: "Failed to connect to docker daemon",
		}
	}

	fp.Attributes["driver.docker"] = pstructs.NewBoolAttribute(true)
	fp.Attributes["driver.docker.version"] = pstructs.NewStringAttribute(env.Get("Version"))
	if d.config.AllowPrivileged {
		fp.Attributes["driver.docker.privileged.enabled"] = pstructs.NewBoolAttribute(true)
	}

	if d.config.Volumes.Enabled {
		fp.Attributes["driver.docker.volumes.enabled"] = pstructs.NewBoolAttribute(true)
	}

	if nets, err := client.ListNetworks(); err != nil {
		d.logger.Warn("error discovering bridge IP", "error", err)
	} else {
		for _, n := range nets {
			if n.Name != "bridge" {
				continue
			}

			if len(n.IPAM.Config) == 0 {
				d.logger.Warn("no IPAM config for bridge network")
				break
			}

			if n.IPAM.Config[0].Gateway != "" {
				fp.Attributes["driver.docker.bridge_ip"] = pstructs.NewStringAttribute(n.IPAM.Config[0].Gateway)
			} else {
				// Docker 17.09.0-ce dropped the Gateway IP from the bridge network
				// See https://github.com/moby/moby/issues/32648
				if !d.fingerprinted() {
					d.logger.Debug("bridge_ip could not be discovered")
				}
			}
			break
		}
	}

	return fp
}
