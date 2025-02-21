// Copyright 2023 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	contextCMD "github.com/okteto/okteto/cmd/context"
	"github.com/okteto/okteto/cmd/utils"
	"github.com/okteto/okteto/pkg/cmd/pipeline"
	oktetoErrors "github.com/okteto/okteto/pkg/errors"
	"github.com/okteto/okteto/pkg/k8s/configmaps"
	oktetoLog "github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/model"
	"github.com/okteto/okteto/pkg/okteto"
	"github.com/okteto/okteto/pkg/types"
	"github.com/spf13/cobra"
)

// deployFlags represents the user input for a pipeline deploy command
type deployFlags struct {
	branch       string
	repository   string
	name         string
	namespace    string
	wait         bool
	skipIfExists bool
	timeout      time.Duration
	file         string
	variables    []string

	// Deprecated fields
	filename string
}

// DeployOptions represents options for deploy pipeline command
type DeployOptions struct {
	Branch       string
	Repository   string
	Name         string
	Namespace    string
	Wait         bool
	SkipIfExists bool
	Timeout      time.Duration
	File         string
	Variables    []string
}

func deploy(ctx context.Context) *cobra.Command {
	flags := &deployFlags{}
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy an okteto pipeline",
		Args:  utils.NoArgsAccepted("https://www.okteto.com/docs/reference/cli/#deploy-1"),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctxResource := &model.ContextResource{}
			if err := ctxResource.UpdateNamespace(flags.namespace); err != nil {
				return err
			}

			ctxOptions := &contextCMD.ContextOptions{
				Namespace: ctxResource.Namespace,
				Show:      true,
			}
			if err := contextCMD.NewContextCommand().Run(ctx, ctxOptions); err != nil {
				return err
			}

			if !okteto.IsOkteto() {
				return oktetoErrors.ErrContextIsNotOktetoCluster
			}

			pipelineCmd, err := NewCommand()
			if err != nil {
				return err
			}
			opts := flags.toOptions()
			return pipelineCmd.ExecuteDeployPipeline(ctx, opts)
		},
	}

	cmd.Flags().StringVarP(&flags.name, "name", "p", "", "name of the pipeline (defaults to the git config name)")
	cmd.Flags().StringVarP(&flags.namespace, "namespace", "n", "", "namespace where the pipeline is deployed (defaults to the current namespace)")
	cmd.Flags().StringVarP(&flags.repository, "repository", "r", "", "the repository to deploy (defaults to the current repository)")
	cmd.Flags().StringVarP(&flags.branch, "branch", "b", "", "the branch to deploy (defaults to the current branch)")
	cmd.Flags().BoolVarP(&flags.wait, "wait", "w", false, "wait until the pipeline finishes (defaults to false)")
	cmd.Flags().BoolVarP(&flags.skipIfExists, "skip-if-exists", "", false, "skip the pipeline deployment if the pipeline already exists in the namespace (defaults to false)")
	cmd.Flags().DurationVarP(&flags.timeout, "timeout", "t", (5 * time.Minute), "the length of time to wait for completion, zero means never. Any other values should contain a corresponding time unit e.g. 1s, 2m, 3h ")
	cmd.Flags().StringArrayVarP(&flags.variables, "var", "v", []string{}, "set a pipeline variable (can be set more than once)")
	cmd.Flags().StringVarP(&flags.file, "file", "f", "", "relative path within the repository to the manifest file (default to okteto-pipeline.yaml or .okteto/okteto-pipeline.yaml)")
	cmd.Flags().StringVarP(&flags.filename, "filename", "", "", "relative path within the repository to the manifest file (default to okteto-pipeline.yaml or .okteto/okteto-pipeline.yaml)")
	cmd.Flags().MarkHidden("filename")
	return cmd
}

// ExecuteDeployPipeline executes deploy pipeline given a set of options
func (pc *Command) ExecuteDeployPipeline(ctx context.Context, opts *DeployOptions) error {

	if err := opts.setDefaults(); err != nil {
		return fmt.Errorf("could not set default values for options: %w", err)
	}

	if opts.SkipIfExists {
		c, _, err := okteto.GetK8sClient()
		if err != nil {
			return fmt.Errorf("failed to load okteto context '%s': %v", okteto.Context().Name, err)
		}

		_, err = configmaps.Get(ctx, pipeline.TranslatePipelineName(opts.Name), opts.Namespace, c)
		if err == nil {
			oktetoLog.Success("Skipping repository '%s' because it's already deployed", opts.Name)
			return nil
		}

		if !oktetoErrors.IsNotFound(err) {
			return err
		}
	}

	resp, err := pc.deployPipeline(ctx, opts)
	if err != nil {
		return err
	}

	if !opts.Wait {
		oktetoLog.Success("Repository '%s' scheduled for deployment", opts.Name)
		return nil
	}

	if err := pc.waitUntilRunning(ctx, opts.Name, opts.Namespace, resp.Action, opts.Timeout); err != nil {
		return err
	}

	oktetoLog.Success("Repository '%s' successfully deployed", opts.Name)
	return nil
}

func (pc *Command) deployPipeline(ctx context.Context, opts *DeployOptions) (*types.GitDeployResponse, error) {
	oktetoLog.Spinner(fmt.Sprintf("Deploying repository '%s'...", opts.Name))
	oktetoLog.StartSpinner()
	defer oktetoLog.StopSpinner()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	exit := make(chan error, 1)

	var resp *types.GitDeployResponse

	go func() {

		pipelineOpts, err := opts.toPipelineDeployClientOptions()
		if err != nil {
			exit <- err
			return
		}
		oktetoLog.Infof("deploy pipeline %s defined on file='%s' repository=%s branch=%s on namespace=%s", opts.Name, opts.File, opts.Repository, opts.Branch, opts.Namespace)

		resp, err = pc.okClient.Pipeline().Deploy(ctx, pipelineOpts)
		exit <- err
	}()

	select {
	case <-stop:
		oktetoLog.Infof("CTRL+C received, starting shutdown sequence")
		return nil, oktetoErrors.ErrIntSig
	case err := <-exit:
		if err != nil {
			oktetoLog.Infof("exit signal received due to error: %s", err)
			return nil, err
		}
	}
	return resp, nil
}

// getPipelineName returns the repository name without sanitizing
func getPipelineName(repository string) string {
	return model.TranslateURLToName(repository)
}

func (pc *Command) streamPipelineLogs(ctx context.Context, name, namespace, actionName string, timeout time.Duration) error {
	// wait to Action be progressing
	if err := pc.okClient.Pipeline().WaitForActionProgressing(ctx, name, namespace, actionName, timeout); err != nil {
		return err
	}

	return pc.okClient.Stream().PipelineLogs(ctx, name, namespace, actionName)
}

func (pc *Command) waitUntilRunning(ctx context.Context, name, namespace string, action *types.Action, timeout time.Duration) error {
	waitCtx, ctxCancel := context.WithCancel(ctx)
	defer ctxCancel()

	oktetoLog.Spinner(fmt.Sprintf("Waiting for repository '%s' to be deployed...", name))
	oktetoLog.StartSpinner()
	defer oktetoLog.StopSpinner()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	exit := make(chan error, 1)

	var wg sync.WaitGroup

	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		err := pc.streamPipelineLogs(waitCtx, name, namespace, action.Name, timeout)
		if err != nil {
			oktetoLog.Warning("pipeline logs cannot be streamed due to connectivity issues")
			oktetoLog.Infof("pipeline logs cannot be streamed due to connectivity issues: %v", err)
		}
	}(&wg)

	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		err := pc.waitToBeDeployed(waitCtx, name, namespace, action, timeout)
		if err != nil {
			exit <- err
			return
		}

		oktetoLog.Spinner("Waiting for containers to be healthy...")
		exit <- pc.waitForResourcesToBeRunning(waitCtx, name, namespace, timeout)
	}(&wg)

	go func(wg *sync.WaitGroup) {
		wg.Wait()
		close(stop)
		close(exit)
	}(&wg)

	select {
	case <-stop:
		ctxCancel()
		oktetoLog.Infof("CTRL+C received, starting shutdown sequence")
		return oktetoErrors.ErrIntSig
	case err := <-exit:
		if err != nil {
			oktetoLog.Infof("exit signal received due to error: %s", err)
			return err
		}
	}

	return nil
}

func (pc *Command) waitToBeDeployed(ctx context.Context, name, namespace string, action *types.Action, timeout time.Duration) error {

	return pc.okClient.Pipeline().WaitForActionToFinish(ctx, name, namespace, action.Name, timeout)
}

func (pc *Command) waitForResourcesToBeRunning(ctx context.Context, name, namespace string, timeout time.Duration) error {
	ticker := time.NewTicker(1 * time.Second)
	to := time.NewTicker(timeout)

	for {
		select {
		case <-to.C:
			return fmt.Errorf("'%s' deploy didn't finish after %s", name, timeout.String())
		case <-ticker.C:
			resourceStatus, err := pc.okClient.Pipeline().GetResourcesStatus(ctx, name, namespace)
			if err != nil {
				return err
			}
			allRunning, err := CheckAllResourcesRunning(name, resourceStatus)
			if err != nil {
				return err
			}
			if allRunning {
				return nil
			}
		}
	}
}

func CheckAllResourcesRunning(name string, resourceStatus map[string]string) (bool, error) {
	allRunning := true
	for resourceID, status := range resourceStatus {
		oktetoLog.Infof("Resource %s is %s", resourceID, status)
		if status == okteto.ErrorStatus {
			return false, fmt.Errorf("repository '%s' deployed with errors", name)
		}
		if okteto.TransitionStatus[status] {
			allRunning = false
		}
	}
	return allRunning, nil
}

func (f deployFlags) toOptions() *DeployOptions {
	file := f.file
	if f.filename != "" {
		oktetoLog.Warning("the 'filename' flag is deprecated and will be removed in a future version. Please consider using 'file' flag")
		if file == "" {
			file = f.filename
		} else {
			oktetoLog.Warning("flags 'filename' and 'file' can not be used at the same time. 'file' flag will take precedence")
		}
	}
	return &DeployOptions{
		Branch:       f.branch,
		Repository:   f.repository,
		Name:         f.name,
		Namespace:    f.namespace,
		Wait:         f.wait,
		SkipIfExists: f.skipIfExists,
		Timeout:      f.timeout,
		File:         file,
		Variables:    f.variables,
	}
}

func (o *DeployOptions) setDefaults() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get the current working directory: %w", err)
	}

	if o.Repository == "" {
		oktetoLog.Info("inferring git repository URL")

		o.Repository, err = model.GetRepositoryURL(cwd)
		if err != nil {
			return fmt.Errorf("could not get repository url: %w", err)
		}
	}

	if o.Name == "" {
		// in case of inferring the name from the repositoryURL
		// opts.Name is not sanitized
		o.Name = getPipelineName(o.Repository)
	}

	currentRepo, err := model.GetRepositoryURL(cwd)
	if err != nil {
		oktetoLog.Debug("cwd does not have .git folder")
	}

	if o.Branch == "" && okteto.AreSameRepository(o.Repository, currentRepo) {

		oktetoLog.Info("inferring git repository branch")
		b, err := utils.GetBranch(cwd)

		if err != nil {
			return err
		}

		o.Branch = b
	}

	if o.Namespace == "" {
		o.Namespace = okteto.Context().Namespace
	}
	return nil
}

func (o *DeployOptions) toPipelineDeployClientOptions() (types.PipelineDeployOptions, error) {
	varList := []types.Variable{}
	for _, v := range o.Variables {
		kv := strings.SplitN(v, "=", 2)
		if len(kv) != 2 {
			return types.PipelineDeployOptions{}, fmt.Errorf("invalid variable value '%s': must follow KEY=VALUE format", v)
		}
		varList = append(varList, types.Variable{
			Name:  kv[0],
			Value: kv[1],
		})
	}
	return types.PipelineDeployOptions{
		Name:       o.Name,
		Repository: o.Repository,
		Branch:     o.Branch,
		Filename:   o.File,
		Variables:  varList,
		Namespace:  o.Namespace,
	}, nil
}
