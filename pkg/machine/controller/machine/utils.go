package machine

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var regExHyphen = regexp.MustCompile("([a-z])([A-Z])")

var (
	RegExMachineDirEnv      = regexp.MustCompile("^" + machineDirEnvKey + ".*")
	RegExMachinePluginToken = regexp.MustCompile("^" + "MACHINE_PLUGIN_TOKEN=" + ".*")
	RegExMachineDriverName  = regexp.MustCompile("^" + "MACHINE_PLUGIN_DRIVER_NAME=" + ".*")
)

const (
	errorCreatingMachine = "Error creating machine: "
	machineDirEnvKey     = "MACHINE_STORAGE_PATH="
	machineCmd           = "docker-machine"
)

func buildCreateCommand(machine *v3.Machine, configMap map[string]interface{}) []string {
	sDriver := strings.ToLower(machine.Status.MachineTemplateSpec.Driver)
	cmd := []string{"create", "-d", sDriver}

	cmd = append(cmd, buildEngineOpts("--engine-install-url", []string{machine.Status.MachineTemplateSpec.EngineInstallURL})...)
	cmd = append(cmd, buildEngineOpts("--engine-opt", mapToSlice(machine.Status.MachineTemplateSpec.EngineOpt))...)
	cmd = append(cmd, buildEngineOpts("--engine-env", mapToSlice(machine.Status.MachineTemplateSpec.EngineEnv))...)
	cmd = append(cmd, buildEngineOpts("--engine-insecure-registry", machine.Status.MachineTemplateSpec.EngineInsecureRegistry)...)
	cmd = append(cmd, buildEngineOpts("--engine-label", mapToSlice(machine.Status.MachineTemplateSpec.EngineLabel))...)
	cmd = append(cmd, buildEngineOpts("--engine-registry-mirror", machine.Status.MachineTemplateSpec.EngineRegistryMirror)...)
	cmd = append(cmd, buildEngineOpts("--engine-storage-driver", []string{machine.Status.MachineTemplateSpec.EngineStorageDriver})...)

	for k, v := range configMap {
		dmField := "--" + sDriver + "-" + strings.ToLower(regExHyphen.ReplaceAllString(k, "${1}-${2}"))
		switch v.(type) {
		case int64:
			cmd = append(cmd, dmField, strconv.FormatInt(v.(int64), 10))
		case string:
			if v.(string) != "" {
				cmd = append(cmd, dmField, v.(string))
			}
		case bool:
			if v.(bool) {
				cmd = append(cmd, dmField)
			}
		case []interface{}:
			for _, s := range v.([]interface{}) {
				if _, ok := s.(string); ok {
					cmd = append(cmd, dmField, s.(string))
				}
			}
		}
	}
	logrus.Debugf("create cmd %v", cmd)
	cmd = append(cmd, machine.Spec.RequestedHostname)
	return cmd
}

func buildEngineOpts(name string, values []string) []string {
	var opts []string
	for _, value := range values {
		if value == "" {
			continue
		}
		opts = append(opts, name, value)
	}
	return opts
}

func mapToSlice(m map[string]string) []string {
	var ret []string
	for k, v := range m {
		ret = append(ret, fmt.Sprintf("%s=%s", k, v))
	}
	return ret
}

func buildCommand(machineDir string, cmdArgs []string) *exec.Cmd {
	command := exec.Command(machineCmd, cmdArgs...)
	env := initEnviron(machineDir)
	command.Env = env
	return command
}

func initEnviron(machineDir string) []string {
	env := os.Environ()
	found := false
	for idx, ev := range env {
		if RegExMachineDirEnv.MatchString(ev) {
			env[idx] = machineDirEnvKey + machineDir
			found = true
		}
		if RegExMachinePluginToken.MatchString(ev) {
			env[idx] = ""
		}
		if RegExMachineDriverName.MatchString(ev) {
			env[idx] = ""
		}
	}
	if !found {
		env = append(env, machineDirEnvKey+machineDir)
	}
	return env
}

func startReturnOutput(command *exec.Cmd) (io.ReadCloser, io.ReadCloser, error) {
	readerStdout, err := command.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}

	readerStderr, err := command.StderrPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := command.Start(); err != nil {
		readerStdout.Close()
		readerStderr.Close()
		return nil, nil, err
	}

	return readerStdout, readerStderr, nil
}

func getSSHKey(machineDir string, obj *v3.Machine) (string, error) {
	if err := waitUntilSSHKey(machineDir, obj); err != nil {
		return "", err
	}

	return getSSHPrivateKey(machineDir, obj)
}

func (m *Lifecycle) reportStatus(stdoutReader io.Reader, stderrReader io.Reader, machine *v3.Machine) (*v3.Machine, error) {
	scanner := bufio.NewScanner(stdoutReader)
	for scanner.Scan() {
		msg := scanner.Text()
		logrus.Infof("stdout: %s", msg)
		_, err := filterDockerMessage(msg, machine)
		if err != nil {
			return machine, err
		}
		m.logger.Info(machine, msg)
		v3.MachineConditionProvisioned.Message(machine, msg)
		// ignore update errors
		if newObj, err := m.machineClient.Update(machine); err == nil {
			machine = newObj
		} else {
			machine, _ = m.machineClient.Get(machine.Name, metav1.GetOptions{})
		}
	}
	scanner = bufio.NewScanner(stderrReader)
	for scanner.Scan() {
		msg := scanner.Text()
		return machine, errors.New(msg)
	}
	return machine, nil
}

func filterDockerMessage(msg string, machine *v3.Machine) (string, error) {
	if strings.Contains(msg, errorCreatingMachine) {
		return "", errors.New(msg)
	}
	if strings.Contains(msg, machine.Spec.RequestedHostname) {
		return "", nil
	}
	return msg, nil
}

func machineExists(machineDir string, name string) (bool, error) {
	command := buildCommand(machineDir, []string{"ls", "-q"})
	r, err := command.StdoutPipe()
	if err != nil {
		return false, err
	}

	err = command.Start()
	if err != nil {
		return false, err
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		foundName := scanner.Text()
		if foundName == name {
			return true, nil
		}
	}
	if err = scanner.Err(); err != nil {
		return false, err
	}

	err = command.Wait()
	if err != nil {
		return false, err
	}

	return false, nil
}

func deleteMachine(machineDir string, machine *v3.Machine) error {
	command := buildCommand(machineDir, []string{"rm", "-f", machine.Spec.RequestedHostname})
	err := command.Start()
	if err != nil {
		return err
	}

	err = command.Wait()
	if err != nil {
		return err
	}

	return nil
}

func getSSHPrivateKey(machineDir string, machine *v3.Machine) (string, error) {
	keyPath := filepath.Join(machineDir, "machines", machine.Spec.RequestedHostname, "id_rsa")
	data, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return "", nil
	}
	return string(data), nil
}

func waitUntilSSHKey(machineDir string, machine *v3.Machine) error {
	keyPath := filepath.Join(machineDir, "machines", machine.Spec.RequestedHostname, "id_rsa")
	startTime := time.Now()
	increments := 1
	for {
		if time.Now().After(startTime.Add(time.Minute * 3)) {
			return errors.New("Timeout waiting for ssh key")
		}
		if _, err := os.Stat(keyPath); err != nil {
			logrus.Debugf("keyPath not found. The machine is probably still provisioning. Sleep %v second", increments)
			time.Sleep(time.Duration(increments) * time.Second)
			increments = increments * 2
			continue
		}
		return nil
	}
}
