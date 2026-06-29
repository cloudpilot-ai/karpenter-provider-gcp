package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

func main() {
	actor := os.Getenv("ACTOR")
	if actor == "" {
		fmt.Fprintln(os.Stderr, "ACTOR is not set")
		os.Exit(1)
	}
	path := os.Getenv("OWNERS_PATH")
	if path == "" {
		path = "OWNERS"
	}

	owners, err := readRootApprovers(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading OWNERS: %v\n", err)
		os.Exit(1)
	}
	allowed := owners[actor]
	if err := setOutput("allowed", fmt.Sprintf("%t", allowed)); err != nil {
		fmt.Fprintf(os.Stderr, "writing output: %v\n", err)
		os.Exit(1)
	}
	if err := setOutput("actor", actor); err != nil {
		fmt.Fprintf(os.Stderr, "writing output: %v\n", err)
		os.Exit(1)
	}
}

func readRootApprovers(path string) (map[string]bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	approvers := map[string]bool{}
	section := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch line {
		case "approvers:":
			section = "approvers"
		case "reviewers:", "emeritus_approvers:", "labels:", "options:", "filters:":
			section = ""
		default:
			if section == "approvers" && strings.HasPrefix(line, "-") {
				login := strings.TrimSpace(strings.TrimPrefix(line, "-"))
				login = strings.Trim(login, `"'`)
				if login != "" {
					approvers[login] = true
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(approvers) == 0 {
		return nil, errors.New("no root approvers found")
	}
	return approvers, nil
}

func setOutput(name, value string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		fmt.Printf("%s=%s\n", name, value)
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%s=%s\n", name, value)
	return err
}
