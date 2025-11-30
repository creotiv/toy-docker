package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/creotiv/toy-docker/internal/build"
	"github.com/creotiv/toy-docker/internal/pull"
	"github.com/creotiv/toy-docker/internal/run"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Commands: pull, build, run, images")
		os.Exit(1)
	}

	switch os.Args[1] {

	case "init":
		run.Init()
		return

	case "pull":
		pullCmd := flag.NewFlagSet("pull", flag.ExitOnError)
		pullCmd.Parse(os.Args[2:])
		if len(pullCmd.Args()) < 1 {
			fmt.Println("usage: toy-docker pull <image[:tag]>")
			os.Exit(1)
		}
		image := pullCmd.Args()[0]
		if err := pull.PullImage(image); err != nil {
			panic(err)
		}

	case "build":
		buildCmd := flag.NewFlagSet("build", flag.ExitOnError)
		buildCmd.Parse(os.Args[2:])
		if len(buildCmd.Args()) != 2 {
			fmt.Println("usage: toy-docker build <Dockerfile> <image-name>")
			os.Exit(1)
		}
		dockerfile := buildCmd.Args()[0]
		name := buildCmd.Args()[1]
		if err := build.BuildImage(dockerfile, name); err != nil {
			panic(err)
		}

	case "run":
		runCmd := flag.NewFlagSet("run", flag.ExitOnError)
		vols := runCmd.String("v", "", "volume mounts host:cont;host2:cont2")
		ports := runCmd.String("p", "", "ports host:cont;")
		runCmd.Parse(os.Args[2:])

		if len(runCmd.Args()) < 1 {
			fmt.Println("usage: toy-docker run <image> [cmd...]")
			os.Exit(1)
		}
		image := runCmd.Args()[0]
		cmd := runCmd.Args()[1:]

		if err := run.RunContainer(image, cmd, *vols, *ports); err != nil {
			panic(err)
		}

	case "images":
		if err := build.ListImages(); err != nil {
			panic(err)
		}

	default:
		fmt.Println("unknown command:", os.Args[1])
		os.Exit(1)
	}
}
