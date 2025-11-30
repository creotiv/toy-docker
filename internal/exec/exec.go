package exec

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func RunOut(desc, name string, args ...string) (string, error) {
	fmt.Println(desc)
	fmt.Println("    >>>>", name, strings.Join(args, " "))
	b, err := exec.Command(name, args...).CombinedOutput()
	return string(b), err
}

func MustRun(desc, name string, args ...string) {
	fmt.Println(desc)
	fmt.Println("    >>>>", name, strings.Join(args, " "))
	if err := Run(name, args...); err != nil {
		panic(fmt.Errorf("%s %v failed: %w", name, args, err))
	}
}

func RunOrErr(desc, name string, args ...string) error {
	fmt.Println(desc)
	fmt.Println("    >>>>", name, strings.Join(args, " "))
	if err := Run(name, args...); err != nil {
		return fmt.Errorf("%s: %w", desc, err)
	}
	return nil
}
