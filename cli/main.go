// AIC CLI — 部署在 PC (Windows/macOS/Linux) 上的 host agent，通过 NATS 连接 AIC 平台。
//
// 自动读取当前目录 .env 文件，命令行参数优先级高于环境变量。
//
// 用法:
//
//	aic -key <env_id>.<cred_ver>.<secret>.<uid>
//	aic -host http://localhost:4000 -key <env_id>.<cred_ver>.<secret>.<uid> -dir /workspace
//
// -host 为平台地址（默认 https://ivec.ai）；-url 可直接指定 NATS WebSocket 端点
// （默认空 = 按 -host 推断：https→wss、http→ws，拼接 /aic/api/nc）。
//
// .env:
//
//	AIC_HOST=https://ivec.ai
//	AIC_URL=
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

	hostURL := getEnv("AIC_HOST", host.DefaultHost)
	natsURL := getEnv("AIC_URL", "") // 空 = 按 -host 推断
	envCred := getEnv("ENV_KEY", "")
	workDir := getEnv("WORK_DIR", "")
	deviceName := getEnv("DEVICE_NAME", "")
	execTimeoutStr := getEnv("EXEC_TIMEOUT", "30m")

	flag.StringVar(&hostURL, "host", hostURL, "AIC platform address (default https://ivec.ai)")
	flag.StringVar(&natsURL, "url", natsURL, "NATS WebSocket endpoint (empty = infer from -host)")
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
		fmt.Fprintln(os.Stderr, "Usage: aic -key <env_id>.<cred_ver>.<secret>.<uid>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Or use .env file:")
		fmt.Fprintln(os.Stderr, "  AIC_HOST     Platform address (default: https://ivec.ai)")
		fmt.Fprintln(os.Stderr, "  AIC_URL      NATS endpoint (empty = infer from AIC_HOST)")
		fmt.Fprintln(os.Stderr, "  ENV_KEY      Environment key from 'env create'")
		fmt.Fprintln(os.Stderr, "  WORK_DIR     Working directory for exec (default: /tmp)")
		fmt.Fprintln(os.Stderr, "  DEVICE_NAME  Device display name (default: hostname)")
		fmt.Fprintln(os.Stderr, "  EXEC_TIMEOUT Exec background timeout (default: 30m)")
		os.Exit(1)
	}

	client := host.New(host.Options{
		Credential:  envCred,
		NATSURL:     host.ResolveNATSURL(hostURL, natsURL),
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
