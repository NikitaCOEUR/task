package compiler

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-task/task/v3/internal/execext"
	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/templater"
	"github.com/go-task/task/v3/internal/version"
	"github.com/go-task/task/v3/taskfile/ast"
)

type Compiler struct {
	Dir            string
	Entrypoint     string
	UserWorkingDir string

	TaskfileEnv  *ast.Vars
	TaskfileVars *ast.Vars

	Logger *logger.Logger

	dynamicCache   map[string]string
	muDynamicCache sync.Mutex
}

func (c *Compiler) GetTaskfileVariables() (*ast.Vars, error) {
	return c.getVariables(nil, nil, true)
}

func (c *Compiler) GetVariables(t *ast.Task, call *ast.Call) (*ast.Vars, error) {
	return c.getVariables(t, call, true)
}

func (c *Compiler) FastGetVariables(t *ast.Task, call *ast.Call) (*ast.Vars, error) {
	return c.getVariables(t, call, false)
}

func (c *Compiler) getVariables(t *ast.Task, call *ast.Call, evaluateShVars bool) (*ast.Vars, error) {
	result := GetEnviron()
	specialVars, err := c.getSpecialVars(t, call)
	if err != nil {
		return nil, err
	}
	for k, v := range specialVars {
		result.Set(k, ast.Var{Value: v})
	}

	getRangeFunc := func(dir string) func(k string, v ast.Var) error {
		return func(k string, v ast.Var) error {
			cache := &templater.Cache{Vars: result}
			// Replace values
			newVar := templater.ReplaceVar(v, cache)
			// If the variable should not be evaluated, but is nil, set it to an empty string
			// This stops empty interface errors when using the templater to replace values later
			if !evaluateShVars && newVar.Value == nil {
				result.Set(k, ast.Var{Value: ""})
				return nil
			}
			// If the variable should not be evaluated and it is set, we can set it and return
			if !evaluateShVars {
				result.Set(k, ast.Var{Value: newVar.Value})
				return nil
			}
			// Now we can check for errors since we've handled all the cases when we don't want to evaluate
			if err := cache.Err(); err != nil {
				return err
			}
			// If the variable is already set, we can set it and return
			if newVar.Value != nil {
				result.Set(k, ast.Var{Value: newVar.Value})
				return nil
			}
			// If the variable is dynamic, we need to resolve it first
			static, err := c.HandleDynamicVar(newVar, dir)
			if err != nil {
				return err
			}
			result.Set(k, ast.Var{Value: static})
			return nil
		}
	}
	rangeFunc := getRangeFunc(c.Dir)

	var taskRangeFunc func(k string, v ast.Var) error
	if t != nil {
		// NOTE(@andreynering): We're manually joining these paths here because
		// this is the raw task, not the compiled one.
		cache := &templater.Cache{Vars: result}
		dir := templater.Replace(t.Dir, cache)
		if err := cache.Err(); err != nil {
			return nil, err
		}
		dir = filepathext.SmartJoin(c.Dir, dir)
		taskRangeFunc = getRangeFunc(dir)
	}

	if err := c.TaskfileEnv.Range(rangeFunc); err != nil {
		return nil, err
	}
	if err := c.TaskfileVars.Range(rangeFunc); err != nil {
		return nil, err
	}
	if t != nil {
		if err := t.IncludeVars.Range(rangeFunc); err != nil {
			return nil, err
		}
		if err := t.IncludedTaskfileVars.Range(taskRangeFunc); err != nil {
			return nil, err
		}
	}

	if t == nil || call == nil {
		return result, nil
	}

	if err := call.Vars.Range(rangeFunc); err != nil {
		return nil, err
	}
	if err := t.Vars.Range(taskRangeFunc); err != nil {
		return nil, err
	}

	return result, nil
}

func (c *Compiler) HandleDynamicVar(v ast.Var, dir string) (string, error) {
	c.muDynamicCache.Lock()
	defer c.muDynamicCache.Unlock()

	// If the variable is not dynamic or it is empty, return an empty string
	if v.Sh == nil || *v.Sh == "" {
		return "", nil
	}

	if c.dynamicCache == nil {
		c.dynamicCache = make(map[string]string, 30)
	}
	if result, ok := c.dynamicCache[*v.Sh]; ok {
		return result, nil
	}

	// NOTE(@andreynering): If a var have a specific dir, use this instead
	if v.Dir != "" {
		dir = v.Dir
	}

	var stdout bytes.Buffer
	opts := &execext.RunCommandOptions{
		Command: *v.Sh,
		Dir:     dir,
		Stdout:  &stdout,
		Stderr:  c.Logger.Stderr,
	}
	if err := execext.RunCommand(context.Background(), opts); err != nil {
		return "", fmt.Errorf(`task: Command "%s" failed: %s`, opts.Command, err)
	}

	// Trim a single trailing newline from the result to make most command
	// output easier to use in shell commands.
	result := strings.TrimSuffix(stdout.String(), "\r\n")
	result = strings.TrimSuffix(result, "\n")

	c.dynamicCache[*v.Sh] = result
	c.Logger.VerboseErrf(logger.Magenta, "task: dynamic variable: %q result: %q\n", *v.Sh, result)

	return result, nil
}

// ResetCache clear the dynamic variables cache
func (c *Compiler) ResetCache() {
	c.muDynamicCache.Lock()
	defer c.muDynamicCache.Unlock()

	c.dynamicCache = nil
}

func (c *Compiler) getSpecialVars(t *ast.Task, call *ast.Call) (map[string]string, error) {
	allVars := map[string]string{
		"TASK_EXE":         filepath.ToSlash(os.Args[0]),
		"ROOT_TASKFILE":    filepathext.SmartJoin(c.Dir, c.Entrypoint),
		"ROOT_DIR":         c.Dir,
		"USER_WORKING_DIR": c.UserWorkingDir,
		"TASK_VERSION":     version.GetVersion(),
	}
	if t != nil {
		maps.Copy(allVars, map[string]string{"TASK": t.Task, "TASKFILE": t.Location.Taskfile, "TASKFILE_DIR": filepath.Dir(t.Location.Taskfile)})
	}
	if call != nil {
		maps.Copy(allVars, map[string]string{"ALIAS": call.Task})
	}
	return allVars, nil
}
