package terraform

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/infracost/infracost/internal/config"
	"github.com/infracost/infracost/internal/schema"
	"github.com/infracost/infracost/internal/ui"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

var defaultTerragruntBinary = "terragrunt"
var minTerragruntVer = "v0.28.1"

type TerragruntProvider struct {
	ctx  *config.ProjectContext
	Path string
	*DirProvider
}

type TerragruntInfo struct {
	ConfigPath string
	WorkingDir string
}

func NewTerragruntProvider(ctx *config.ProjectContext) schema.Provider {
	dirProvider := NewDirProvider(ctx).(*DirProvider)

	terragruntBinary := ctx.ProjectConfig.TerraformBinary
	if terragruntBinary == "" {
		terragruntBinary = defaultTerragruntBinary
	}

	dirProvider.TerraformBinary = terragruntBinary
	dirProvider.IsTerragrunt = true

	return &TerragruntProvider{
		ctx:         ctx,
		DirProvider: dirProvider,
		Path:        ctx.ProjectConfig.Path,
	}
}

func (p *TerragruntProvider) Type() string {
	return "terragrunt"
}

func (p *TerragruntProvider) DisplayType() string {
	return "Terragrunt directory"
}

func (p *TerragruntProvider) AddMetadata(metadata *schema.ProjectMetadata) {
	// no op
}

func (p *TerragruntProvider) LoadResources(usage map[string]*schema.UsageData) ([]*schema.Project, error) {
	// We want to run Terragrunt commands from the config dirs
	// Terragrunt internally runs Terraform in the working dirs, so we need to be aware of these
	// so we can handle reading and cleaning up the generated plan files.
	configDirs, workingDirs, err := p.getProjectDirs()

	if err != nil {
		return []*schema.Project{}, err
	}

	var outs [][]byte

	if p.UseState {
		outs, err = p.generateStateJSONs(configDirs)
	} else {
		outs, err = p.generatePlanJSONs(configDirs, workingDirs)
	}
	if err != nil {
		return []*schema.Project{}, err
	}

	projects := make([]*schema.Project, 0, len(configDirs))

	for i, path := range configDirs {
		metadata := config.DetectProjectMetadata(path)
		metadata.Type = p.Type()
		p.AddMetadata(metadata)
		name := schema.GenerateProjectName(metadata, p.ctx.RunContext.Config.EnableDashboard)

		project := schema.NewProject(name, metadata)

		parser := NewParser(p.ctx)
		pastResources, resources, err := parser.parseJSON(outs[i], usage)
		if err != nil {
			return projects, errors.Wrap(err, "Error parsing Terraform JSON")
		}

		project.HasDiff = !p.UseState
		if project.HasDiff {
			project.PastResources = pastResources
		}
		project.Resources = resources

		projects = append(projects, project)
	}

	return projects, nil
}

func (p *TerragruntProvider) getProjectDirs() ([]string, []string, error) {
	spinner := ui.NewSpinner("Running terragrunt run-all terragrunt-info", p.spinnerOpts)

	opts := &CmdOptions{
		TerraformBinary: p.TerraformBinary,
		Dir:             p.Path,
	}
	out, err := Cmd(opts, "run-all", "--terragrunt-ignore-external-dependencies", "terragrunt-info")
	if err != nil {
		spinner.Fail()
		p.printTerraformErr(err)

		return []string{}, []string{}, err
	}

	jsons := bytes.SplitAfter(out, []byte{'}', '\n'})
	if len(jsons) > 1 {
		jsons = jsons[:len(jsons)-1]
	}

	configDirs := make([]string, 0, len(jsons))
	workingDirs := make([]string, 0, len(jsons))

	for _, j := range jsons {
		var info TerragruntInfo
		err = json.Unmarshal(j, &info)
		if err != nil {
			spinner.Fail()
			return configDirs, workingDirs, err
		}

		configDirs = append(configDirs, filepath.Dir(info.ConfigPath))
		workingDirs = append(workingDirs, info.WorkingDir)
	}

	spinner.Success()

	return configDirs, workingDirs, nil
}

func (p *TerragruntProvider) generateStateJSONs(configDirs []string) ([][]byte, error) {
	err := p.checks()
	if err != nil {
		return [][]byte{}, err
	}

	outs := make([][]byte, 0, len(configDirs))

	spinnerMsg := "Running terragrunt show"
	if len(configDirs) > 1 {
		spinnerMsg += " for each project"
	}
	spinner := ui.NewSpinner(spinnerMsg, p.spinnerOpts)

	for _, path := range configDirs {
		opts, err := p.buildCommandOpts(path)
		if err != nil {
			return [][]byte{}, err
		}
		if opts.TerraformConfigFile != "" {
			defer os.Remove(opts.TerraformConfigFile)
		}

		out, err := p.runShow(opts, spinner, "")
		if err != nil {
			return outs, err
		}
		outs = append(outs, out)
	}

	return outs, nil
}

func (p *DirProvider) generatePlanJSONs(configDirs []string, workingDirs []string) ([][]byte, error) {
	err := p.checks()
	if err != nil {
		return [][]byte{}, err
	}

	opts, err := p.buildCommandOpts(p.Path)
	if err != nil {
		return [][]byte{}, err
	}
	if opts.TerraformConfigFile != "" {
		defer os.Remove(opts.TerraformConfigFile)
	}

	spinner := ui.NewSpinner("Running terragrunt run-all plan", p.spinnerOpts)
	planFile, planJSON, err := p.runPlan(opts, spinner, true)
	defer func() {
		err := cleanupPlanFiles(workingDirs, planFile)
		if err != nil {
			log.Warnf("Error cleaning up plan files: %v", err)
		}
	}()

	if err != nil {
		return [][]byte{}, err
	}

	if len(planJSON) > 0 {
		return [][]byte{planJSON}, nil
	}

	outs := make([][]byte, 0, len(configDirs))
	spinnerMsg := "Running terragrunt show"
	if len(configDirs) > 1 {
		spinnerMsg += " for each project"
	}
	spinner = ui.NewSpinner(spinnerMsg, p.spinnerOpts)

	for i, path := range configDirs {
		opts, err := p.buildCommandOpts(path)
		if err != nil {
			return [][]byte{}, err
		}
		if opts.TerraformConfigFile != "" {
			defer os.Remove(opts.TerraformConfigFile)
		}

		out, err := p.runShow(opts, spinner, filepath.Join(workingDirs[i], planFile))
		if err != nil {
			return outs, err
		}
		outs = append(outs, out)
	}

	return outs, nil
}

func cleanupPlanFiles(paths []string, planFile string) error {
	if planFile == "" {
		return nil
	}

	for _, path := range paths {
		err := os.Remove(filepath.Join(path, planFile))
		if err != nil {
			return err
		}
	}

	return nil
}
