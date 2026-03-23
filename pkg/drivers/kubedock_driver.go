// Copyright 2017 Google Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drivers

import (
	"archive/tar"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/GoogleContainerTools/container-structure-test/internal/pkgutil"
	"github.com/GoogleContainerTools/container-structure-test/pkg/types/unversioned"

	docker "github.com/fsouza/go-dockerclient"
)

type KubeDockDriver struct {
	image            string
	currentContainer *docker.Container
	cli              docker.Client
	env              map[string]string
	save             bool
	runtime          string
	platform         string
	runOpts          unversioned.ContainerRunOptions
}

func NewKubeDockDriver(args DriverConfig) (Driver, error) {
	newCli, err := docker.NewClientFromEnv()
	if err != nil {
		return nil, err
	}
	return &KubeDockDriver{
		image:            args.Image,
		currentContainer: nil,
		cli:              *newCli,
		env:              nil,
		save:             args.Save,
		runtime:          args.Runtime,
		platform:         args.Platform,
		runOpts:          args.RunOpts,
	}, nil
}

func (d *KubeDockDriver) hostConfig() *docker.HostConfig {
	if d.runOpts.IsSet() && d.runtime != "" {
		return &docker.HostConfig{
			Capabilities: d.runOpts.Capabilities,
			Binds:        d.runOpts.BindMounts,
			Privileged:   d.runOpts.Privileged,
			Runtime:      d.runtime,
		}
	}
	if d.runOpts.IsSet() {
		return &docker.HostConfig{
			Capabilities: d.runOpts.Capabilities,
			Binds:        d.runOpts.BindMounts,
			Privileged:   d.runOpts.Privileged,
		}
	}
	if d.runtime != "" {
		return &docker.HostConfig{
			Runtime: d.runtime,
		}
	}
	return nil
}

func (d *KubeDockDriver) Destroy() {
	if d.currentContainer != nil {
		// Stop container if any
		err := d.cli.StopContainer(d.currentContainer.ID, 5)
		if err != nil {
			logrus.Warnf("failed stopping container: %s", err)
		}
	}
}

func (d *KubeDockDriver) SetEnv(envVars []unversioned.EnvVar) error {
	if len(envVars) == 0 {
		return nil
	}

	//TODO execute export?

	return nil
}

func (d *KubeDockDriver) start() error {
	if d.currentContainer == nil {
		// spin up a pod that sleeps
		container, err := d.cli.CreateContainer(docker.CreateContainerOptions{
			Platform: d.platform,
			Config: &docker.Config{
				Image: d.image,
				Cmd:   []string{"sleep", "99999"},
			},
			HostConfig:       d.hostConfig(),
			NetworkingConfig: nil,
		})
		if err != nil {
			return errors.Wrap(err, "Error creating container")
		}

		if err = d.cli.StartContainer(container.ID, nil); err != nil {
			return errors.Wrap(err, "Error creating container")
		}

		// if _, err = d.cli.WaitContainer(container.ID); err != nil {
		// 	return errors.Wrap(err, "Error when waiting for container")
		// }

		d.currentContainer = container
	}
	return nil
}

func (d *KubeDockDriver) Setup(envVars []unversioned.EnvVar, fullCommands [][]string) error {
	err := d.start()
	if err != nil {
		return err
	}

	for _, cmd := range fullCommands {
		_, _, _, err := d.exec(d.processEnvVars(envVars), cmd)
		if err != nil {
			return errors.Wrap(err, "Error while executing command on container")
		}
	}

	return nil
}

func (d *KubeDockDriver) Teardown(_ [][]string) error {
	// not required
	return nil
}

func (d *KubeDockDriver) ProcessCommand(envVars []unversioned.EnvVar, fullCommand []string) (string, string, int, error) {
	var env []string
	for _, envVar := range envVars {
		env = append(env, fmt.Sprintf("%s=%s", envVar.Key, envVar.Value))
	}
	stdout, stderr, exitCode, err := d.exec(env, fullCommand)
	if err != nil {
		return "", "", -1, err
	}

	if stdout != "" {
		logrus.Infof("stdout: %s", stdout)
	}
	if stderr != "" {
		logrus.Infof("stderr: %s", stderr)
	}
	return stdout, stderr, exitCode, nil
}

// copies a tar archive starting at the specified path from the image, and returns
// a tar reader which can be used to iterate through its contents and retrieve metadata
func (d *KubeDockDriver) retrieveTar(path string) (*tar.Reader, error) {
	var err error
	var b bytes.Buffer

	err = d.start()
	if err != nil {
		return nil, err
	}

	stream := bufio.NewWriter(&b)

	if err = d.cli.DownloadFromContainer(d.currentContainer.ID, docker.DownloadFromContainerOptions{
		OutputStream: stream,
		Path:         path,
	}); err != nil {
		return nil, errors.Wrap(err, "Error retrieving file from container")
	}
	if err = stream.Flush(); err != nil {
		return nil, err
	}

	return tar.NewReader(bytes.NewReader(b.Bytes())), nil
}

func (d *KubeDockDriver) StatFile(target string) (os.FileInfo, error) {
	reader, err := d.retrieveTar(target)
	if err != nil {
		return nil, err
	}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		switch header.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeLink, tar.TypeSymlink:
			if filepath.Clean(header.Name) == path.Base(target) {
				return header.FileInfo(), nil
			}
		default:
			continue
		}
	}
	return nil, fmt.Errorf("File %s not found in image", target)
}

func (d *KubeDockDriver) ReadFile(target string) ([]byte, error) {
	reader, err := d.retrieveTar(target)
	if err != nil {
		return nil, err
	}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if filepath.Clean(header.Name) == path.Base(target) {
				return nil, fmt.Errorf("Cannot read specified path: %s is a directory, not a file", target)
			}
		case tar.TypeSymlink:
			return d.ReadFile(header.Linkname)
		case tar.TypeReg, tar.TypeLink:
			if filepath.Clean(header.Name) == path.Base(target) {
				var b bytes.Buffer
				stream := bufio.NewWriter(&b)
				io.Copy(stream, reader)
				return b.Bytes(), nil
			}
		default:
			continue
		}
	}
	return nil, fmt.Errorf("File %s not found in image", target)
}

func (d *KubeDockDriver) ReadDir(target string) ([]os.FileInfo, error) {
	reader, err := d.retrieveTar(target)
	if err != nil {
		return nil, err
	}
	var infos []os.FileInfo
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if header.Typeflag == tar.TypeDir {
			// we only want top level dirs here, no recursion. to get these, remove
			// trailing separator and split on separator. there should only be two parts.
			parts := strings.Split(strings.TrimSuffix(header.Name, string(os.PathSeparator)), string(os.PathSeparator))
			if len(parts) == 2 {
				infos = append(infos, header.FileInfo())
			}
		}
	}
	return infos, nil
}

func (d *KubeDockDriver) GetConfig() (unversioned.Config, error) {
	img, err := pkgutil.GetImageForName("remote://" + d.image)
	if err != nil {
		return unversioned.Config{}, errors.Wrap(err, "retrieving image")
	}

	configFile, err := img.Image.ConfigFile()
	if err != nil {
		return unversioned.Config{}, errors.Wrap(err, "retrieving config file")
	}
	config := configFile.Config

	// docker provides these as maps (since they can be mapped in docker run commands)
	// since this will never be the case when built through a dockerfile, we convert to list of strings
	volumes := []string{}
	for v := range config.Volumes {
		volumes = append(volumes, v)
	}

	ports := []string{}
	for p := range config.ExposedPorts {
		// docker always appends the protocol to the port, so this is safe
		ports = append(ports, strings.Split(p, "/")[0])
	}

	return unversioned.Config{
		Env:          convertSliceToMap(config.Env),
		Entrypoint:   config.Entrypoint,
		Cmd:          config.Cmd,
		Volumes:      volumes,
		Workdir:      config.WorkingDir,
		ExposedPorts: ports,
		Labels:       config.Labels,
		User:         config.User,
	}, nil
}

func (d *KubeDockDriver) exec(env []string, command []string) (string, string, int, error) {
	if d.runOpts.EnvFile != "" {
		varMap, err := godotenv.Read(d.runOpts.EnvFile)
		if err != nil {
			logrus.Warnf("Unable to load envFile %s: %s", d.runOpts.EnvFile, err.Error())
		} else {
			var varsFromFile []string
			for k, v := range varMap {
				if k != "" && v != "" {
					varsFromFile = append(varsFromFile, fmt.Sprintf("%s=%s", k, v))
				}
			}
			env = append(env, varsFromFile...)
		}
	}

	if d.runOpts.EnvVars != nil && len(d.runOpts.EnvVars) > 0 {
		varsFromEnv := make([]string, len(d.runOpts.EnvVars))
		for i, e := range d.runOpts.EnvVars {
			v := os.Getenv(e)
			if v != "" {
				varsFromEnv[i] = fmt.Sprintf("%s=%s", e, v)
			}
		}
		env = append(env, varsFromEnv...)
	}

	createOpts := docker.CreateExecOptions{
		Cmd:          command,
		Container:    d.currentContainer.ID,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          env,
	}

	if d.runOpts.IsSet() {
		createOpts.Tty = d.runOpts.TTY
		if len(d.runOpts.User) > 0 {
			createOpts.User = d.runOpts.User
		}
	}

	// first, start container from the current image
	exec, err := d.cli.CreateExec(createOpts)
	if err != nil {
		return "", "", -1, errors.Wrap(err, "Error exec in container")
	}

	var stdout, stderr bytes.Buffer

	err = d.cli.StartExec(exec.ID, docker.StartExecOptions{OutputStream: &stdout, ErrorStream: &stderr, Detach: false})
	if err != nil {
		return "", "", -1, errors.Wrap(err, "Error starting exec in container")
	}

	inspect, err := d.cli.InspectExec(exec.ID)
	if err != nil {
		return "", "", -1, errors.Wrap(err, "Error inspecting result of exec in container")
	}

	return stdout.String(), stderr.String(), inspect.ExitCode, nil
}

func retrieveKubeDockEnv(d *KubeDockDriver) func(string) string {
	return func(envVar string) string {
		var env map[string]string
		if env == nil {
			image, err := d.cli.InspectImage(d.image)
			if err != nil {
				return ""
			}
			// convert env to map for processing
			env = convertSliceToMap(image.Config.Env)
		}
		return env[envVar]
	}
}

// returns the value associated with the provided key in the image's environment
func (d *KubeDockDriver) retrieveEnvVar(envVar string) string {
	// since we're only retrieving these during processing, we can use a closure to cache this
	return retrieveKubeDockEnv(d)(envVar)
}

func (d *KubeDockDriver) processEnvVars(vars []unversioned.EnvVar) []string {
	if len(vars) == 0 {
		return nil
	}

	env := []string{}

	for _, envVar := range vars {
		expandedVal := os.Expand(envVar.Value, d.retrieveEnvVar)
		env = append(env, fmt.Sprintf("%s=%s", envVar.Key, expandedVal))
	}
	return env
}
