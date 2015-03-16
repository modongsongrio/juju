package cloudinit

import (
	"github.com/juju/errors"
	"github.com/juju/utils/shell"
	"gopkg.in/yaml.v1"

	"github.com/juju/juju/version"
)

// Renderer is used to render a cloud-init config into the corresponding
// script to write to disk.
type Renderer struct {
	shell.Renderer

	render func(conf *Config) ([]byte, error)
}

// NewRenderer returns a new cloudinit script renderer for the
// requested series.
func NewRenderer(series string) (*Renderer, error) {
	operatingSystem, err := version.GetOSFromSeries(series)
	if err != nil {
		return nil, errors.Trace(err)
	}

	renderer := &Renderer{}
	switch operatingSystem {
	case version.Windows:
		renderer.Renderer = &shell.PowershellRenderer{}
		renderer.render = powershellRender
	case version.Ubuntu:
		renderer.Renderer = &shell.BashRenderer{}
		renderer.render = ubuntuRender
	default:
		return nil, errors.Errorf("No renderer could be found for %s", series)
	}
	return renderer, nil
}

// Render renders the userdata script for a particular OS type.
func (r Renderer) Render(conf *Config) ([]byte, error) {
	return r.render(conf)
}

func ubuntuRender(conf *Config) ([]byte, error) {
	data, err := yaml.Marshal(conf.attrs)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), data...), nil
}

func powershellRender(conf *Config) ([]byte, error) {
	winCmds := conf.attrs["runcmd"]
	var script []byte
	newline := "\r\n"
	header := "#ps1_sysnative\r\n"
	script = append(script, header...)
	for _, value := range winCmds.([]*command) {
		script = append(script, newline...)
		script = append(script, value.literal...)

	}
	return script, nil
}
