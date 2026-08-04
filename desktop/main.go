// AIC Desktop 客户端 — 部署在 PC (Windows/macOS/Linux) 上，通过 NATS 连接 AIC 服务端。
//
// 自动读取当前目录 .env 文件，命令行参数优先级高于环境变量。
//
// 用法:
//
//	aic
//	aic -url wss://ivec.ai/aic/api/nc -dir /workspace
//
// .env:
//
//	AIC_URL=wss://ivec.ai/aic/api/nc
//	ENV_KEY=<env_id>.<cred_ver>.<secret>.<uid>
//	WORK_DIR=/workspace
//	DEVICE_NAME=my-server
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/veypi/aic-pod/sdk/host"
)

var version = "v0.5.1"

func main() {
	_ = godotenv.Load()

	natsURL := getEnv("AIC_URL", "wss://ivec.ai/aic/api/nc")
	envCred := getEnv("ENV_KEY", "")
	workDir := getEnv("WORK_DIR", "")
	deviceName := getEnv("DEVICE_NAME", "")
	execTimeoutStr := getEnv("EXEC_TIMEOUT", "30m")

	flag.StringVar(&natsURL, "url", natsURL, "AIC server URL")
	flag.StringVar(&envCred, "key", envCred, "Environment key (<env_id>.<cred_ver>.<secret>.<uid>)")
	flag.StringVar(&workDir, "dir", workDir, "Working directory for exec (default /tmp)")
	flag.StringVar(&deviceName, "name", deviceName, "Device display name (default hostname)")
	flag.StringVar(&execTimeoutStr, "exec-timeout", execTimeoutStr, "Exec background timeout (default 30m)")
	showVersion := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("aic-pod %s\n", version)
		os.Exit(0)
	}

	execTimeout, err := time.ParseDuration(execTimeoutStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid exec-timeout %q: %v\n", execTimeoutStr, err)
		os.Exit(1)
	}

	if envCred == "" {
		fmt.Fprintln(os.Stderr, "Usage: ENV_KEY=<env_id>.<cred_ver>.<secret>.<uid> aic")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Or use .env file:")
		fmt.Fprintln(os.Stderr, "  AIC_URL      Server URL (default: wss://ivec.ai/aic/api/nc)")
		fmt.Fprintln(os.Stderr, "  ENV_KEY      Environment key from 'env create'")
		fmt.Fprintln(os.Stderr, "  WORK_DIR     Working directory for exec (default: /tmp)")
		fmt.Fprintln(os.Stderr, "  DEVICE_NAME  Device display name (default: hostname)")
		fmt.Fprintln(os.Stderr, "  EXEC_TIMEOUT Exec background timeout (default: 30m)")
		os.Exit(1)
	}

	client := host.New(host.Options{
		Credential:  envCred,
		NATSURL:     natsURL,
		WorkDir:     workDir,
		DeviceName:  deviceName,
		Version:     version,
		ExecTimeout: execTimeout,
	})

	if err := client.Connect(); err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}

	// 阻塞等待 SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("shutting down...")
	client.Close()
}

func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
