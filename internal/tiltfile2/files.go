package tiltfile2

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/skylark"
	"github.com/pkg/errors"

	"github.com/windmilleng/tilt/internal/kustomize"
)

type gitRepo struct {
	basePath             string
	gitignoreContents    string
	dockerignoreContents string
}

func (s *tiltfileState) newGitRepo(path string) (*gitRepo, error) {
	absPath := s.absPath(path)
	_, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("Reading path %s: %v", path, err)
	}

	if _, err := os.Stat(filepath.Join(absPath, ".git")); os.IsNotExist(err) {
		return nil, fmt.Errorf("%s isn't a valid git repo: it doesn't have a .git/ directory", absPath)
	}

	gitignoreContents, err := ioutil.ReadFile(filepath.Join(absPath, ".gitignore"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	dockerignoreContents, err := ioutil.ReadFile(filepath.Join(absPath, ".dockerignore"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	return &gitRepo{absPath, string(gitignoreContents), string(dockerignoreContents)}, nil
}

func (s *tiltfileState) localGitRepo(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var path string
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "path", &path)
	if err != nil {
		return nil, err
	}

	return s.newGitRepo(path)
}

var _ skylark.Value = &gitRepo{}

func (gr *gitRepo) String() string {
	return fmt.Sprintf("[gitRepo] '%v'", gr.basePath)
}

func (gr *gitRepo) Type() string {
	return "gitRepo"
}

func (gr *gitRepo) Freeze() {}

func (gr *gitRepo) Truth() skylark.Bool {
	return gr.basePath != "" || gr.gitignoreContents != "" || gr.dockerignoreContents != ""
}

func (*gitRepo) Hash() (uint32, error) {
	return 0, errors.New("unhashable type: gitRepo")
}

func (gr *gitRepo) Attr(name string) (skylark.Value, error) {
	switch name {
	case "path":
		return skylark.NewBuiltin(name, gr.path), nil
	default:
		return nil, nil
	}

}

func (gr *gitRepo) AttrNames() []string {
	return []string{"path"}
}

func (gr *gitRepo) path(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var path string
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "path", &path)
	if err != nil {
		return nil, err
	}

	return gr.makeLocalPath(path), nil
}

func (gr *gitRepo) makeLocalPath(path string) localPath {
	return localPath{filepath.Join(gr.basePath, path), gr}
}

type localPath struct {
	path string
	repo *gitRepo
}

func (s *tiltfileState) localPathFromSkylarkValue(v skylark.Value) (localPath, error) {
	switch v := v.(type) {
	case localPath:
		return v, nil
	case *gitRepo:
		return v.makeLocalPath("."), nil
	case skylark.String:
		return localPath{path: s.absPath(string(v))}, nil
	default:
		return localPath{}, fmt.Errorf("Expected local path. Actual type: %T", v)
	}
}

var _ skylark.Value = localPath{}

func (lp localPath) String() string {
	return lp.path
}

func (localPath) Type() string {
	return "localPath"
}

func (localPath) Freeze() {}

func (localPath) Hash() (uint32, error) {
	return 0, errors.New("unhashable type: localPath")
}

func (lp localPath) Truth() skylark.Bool {
	return lp != localPath{}
}

func (lp localPath) join(path string) localPath {
	return localPath{path: filepath.Join(lp.path, path), repo: lp.repo}
}

// When running the Tilt demo, the current working directory is arbitrary.
// So we want to resolve paths relative to the dir where the Tiltfile lives,
// not relative to the working directory.
func (s *tiltfileState) absPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.absWorkingDir(), path)
}

func (s *tiltfileState) absWorkingDir() string {
	return filepath.Dir(s.filename)
}

func (s *tiltfileState) recordConfigFile(f string) {
	s.configFiles = append(s.configFiles, f)
}

func (s *tiltfileState) readFile(p localPath) ([]byte, error) {
	s.recordConfigFile(p.path)
	return ioutil.ReadFile(p.path)
}

func (s *tiltfileState) skylarkReadFile(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var path skylark.Value
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "path", &path)
	if err != nil {
		return nil, err
	}

	p, err := s.localPathFromSkylarkValue(path)
	if err != nil {
		return nil, fmt.Errorf("invalid type for path: %v", err)
	}

	bs, err := s.readFile(p)
	if err != nil {
		return nil, err
	}

	return newBlob(string(bs)), nil
}

type blob struct {
	text string
}

var _ skylark.Value = &blob{}

func newBlob(text string) *blob {
	return &blob{text: text}
}

func (b *blob) String() string {
	return b.text
}

func (b *blob) Type() string {
	return "blob"
}

func (b *blob) Freeze() {}

func (b *blob) Truth() skylark.Bool {
	return len(b.text) > 0
}

func (b *blob) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: blob")
}

func (s *tiltfileState) local(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var command string
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "command", &command)
	if err != nil {
		return nil, err
	}

	out, err := s.execLocalCmd(command)
	if err != nil {
		return nil, err
	}

	return newBlob(out), nil
}

func (s *tiltfileState) execLocalCmd(cmd string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	c.Dir = filepath.Dir(s.filename)
	out, err := c.Output()
	if err != nil {
		errorMessage := fmt.Sprintf("command '%v' failed.\nerror: '%v'\nstdout: '%v'", cmd, err, string(out))
		exitError, ok := err.(*exec.ExitError)
		if ok {
			errorMessage += fmt.Sprintf("\nstderr: '%v'", string(exitError.Stderr))
		}
		return "", errors.New(errorMessage)
	}

	return string(out), nil
}

func (s *tiltfileState) kustomize(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var path skylark.Value
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "path", &path)
	if err != nil {
		return nil, err
	}

	kustomizePath, err := s.localPathFromSkylarkValue(path)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (path): %v", err)
	}

	cmd := fmt.Sprintf("kustomize build %s", path)
	yaml, err := s.execLocalCmd(cmd)
	if err != nil {
		return nil, err
	}
	deps, err := kustomize.Deps(kustomizePath.String())
	if err != nil {
		return nil, fmt.Errorf("internal error: %v", err)
	}
	for _, d := range deps {
		s.recordConfigFile(d)
	}

	return newBlob(string(yaml)), nil
}
