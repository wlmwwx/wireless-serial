// Command wserial is the host-side companion of the wireless-serial ESP32-C3
// firmware. It turns a device's TCP port into a local virtual serial port
// (macOS/Linux PTY symlink, Windows com0com pair) or into a terminal console.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/wireless-serial/wserial/internal/bridge"
	"github.com/wireless-serial/wserial/internal/discovery"
	wlog "github.com/wireless-serial/wserial/internal/log"
	"github.com/wireless-serial/wserial/internal/vport"
	"github.com/wireless-serial/wserial/internal/wsp"
)

// Build metadata; override with -ldflags "-X main.version=...".
var (
	version   = "0.1.0"
	commit    = "dev"
	buildDate = "unknown"
)

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNoDevice = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printGlobalUsage(os.Stderr)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		printGlobalUsage(os.Stdout)
		return exitOK
	case "version", "-V", "--version":
		printVersion(os.Stdout)
		return exitOK
	case "list":
		return cmdList(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "monitor":
		return cmdMonitor(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wserial: 未知子命令 %q（可用：list, run, monitor, version）\n\n", args[0])
		printGlobalUsage(os.Stderr)
		return exitUsage
	}
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "wserial %s\n", version)
	fmt.Fprintf(w, "WSP/1 无线串口主机端工具（%s/%s）\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "默认端口: WSP=%d RAW=%d\n", wsp.DefaultWSPPort, wsp.DefaultRAWPort)
	fmt.Fprintf(w, "构建: commit=%s date=%s\n", commit, buildDate)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		timeout time.Duration
		asJSON  bool
	)
	fs.DurationVar(&timeout, "timeout", 3*time.Second, "mDNS 搜索时长")
	fs.BoolVar(&asJSON, "json", false, "以 JSON 输出")
	fs.Usage = func() { printListHelp(os.Stdout) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "wserial list: %v\n\n", err)
		printListHelp(os.Stderr)
		return exitUsage
	}
	if timeout <= 0 {
		fmt.Fprintln(os.Stderr, "wserial list: --timeout 必须大于 0")
		return exitUsage
	}

	logger := wlog.New(os.Stderr, wlog.LevelInfo)
	devs, err := discovery.Discover(context.Background(), timeout, logger)
	if err != nil {
		logger.Errorf("%v", err)
		return exitError
	}
	if len(devs) == 0 {
		if asJSON {
			out, _ := discovery.FormatJSON(nil)
			fmt.Fprint(os.Stdout, out)
		}
		fmt.Fprintf(os.Stderr, "未发现 %s 设备（请确认设备已上电并加入同一 WiFi）\n", discovery.ServiceType)
		return exitNoDevice
	}
	if asJSON {
		out, err := discovery.FormatJSON(devs)
		if err != nil {
			logger.Errorf("生成 JSON 失败: %v", err)
			return exitError
		}
		fmt.Fprint(os.Stdout, out)
		return exitOK
	}
	fmt.Fprint(os.Stdout, discovery.FormatTable(devs))
	return exitOK
}

// ---------------------------------------------------------------------------
// common flags for run / monitor
// ---------------------------------------------------------------------------

type commonFlags struct {
	host          string
	port          int
	portSet       bool
	device        string
	baud          uint
	dataBits      int
	parity        string
	stopBits      int
	flow          string
	raw           bool
	noReconnect   bool
	resetOnStart  bool
	pingInterval  time.Duration
	verbose       bool
	logLevel      string
	logLevelSet   bool
	explicitFlags map[string]bool
}

func addCommonFlags(fs *flag.FlagSet, cf *commonFlags) {
	fs.StringVar(&cf.host, "H", "", "设备 IP/主机名；留空则 mDNS 自动发现")
	fs.StringVar(&cf.host, "host", "", "设备 IP/主机名；留空则 mDNS 自动发现")
	fs.IntVar(&cf.port, "P", wsp.DefaultWSPPort, "TCP 端口")
	fs.IntVar(&cf.port, "port", wsp.DefaultWSPPort, "TCP 端口")
	fs.StringVar(&cf.device, "d", "", "虚拟串口路径（Unix 默认 /tmp/ttyWS0；Windows 必填，如 COM12）")
	fs.StringVar(&cf.device, "device", "", "虚拟串口路径（Unix 默认 /tmp/ttyWS0；Windows 必填，如 COM12）")
	fs.UintVar(&cf.baud, "b", 115200, "目标波特率；0 = 保持设备当前值")
	fs.UintVar(&cf.baud, "baud", 115200, "目标波特率；0 = 保持设备当前值")
	fs.IntVar(&cf.dataBits, "databits", 8, "数据位 5..8")
	fs.StringVar(&cf.parity, "parity", "none", "校验位 none|odd|even")
	fs.IntVar(&cf.stopBits, "stopbits", 1, "停止位 1|2")
	fs.StringVar(&cf.flow, "flow", "none", "流控 none|rtscts")
	fs.BoolVar(&cf.raw, "raw", false, "RAW 模式（端口 3334，纯字节透传，无握手）")
	fs.BoolVar(&cf.noReconnect, "no-reconnect", false, "断开后不自动重连")
	fs.BoolVar(&cf.resetOnStart, "reset-on-start", false, "连接后向目标发送 120ms 复位脉冲")
	fs.DurationVar(&cf.pingInterval, "ping-interval", 15*time.Second, "WSP 心跳间隔；0 = 关闭")
	fs.BoolVar(&cf.verbose, "v", false, "等价于 --log-level debug")
	fs.BoolVar(&cf.verbose, "verbose", false, "等价于 --log-level debug")
	fs.StringVar(&cf.logLevel, "log-level", "info", "日志级别 error|warn|info|debug|trace")
}

// noteExplicit records which flags appeared on the command line.
func (cf *commonFlags) noteExplicit(fs *flag.FlagSet) {
	cf.explicitFlags = make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		cf.explicitFlags[f.Name] = true
		switch f.Name {
		case "P", "port":
			cf.portSet = true
		case "log-level":
			cf.logLevelSet = true
		}
	})
}

// logger builds the stderr logger from --log-level / --verbose.
func (cf *commonFlags) logger() (*wlog.Logger, error) {
	levelName := cf.logLevel
	if !cf.logLevelSet && cf.verbose {
		levelName = "debug"
	}
	lvl, err := wlog.ParseLevel(levelName)
	if err != nil {
		return nil, err
	}
	return wlog.New(os.Stderr, lvl), nil
}

func parseParity(s string) (uint8, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "n", "0":
		return wsp.ParityNone, nil
	case "odd", "o", "1":
		return wsp.ParityOdd, nil
	case "even", "e", "2":
		return wsp.ParityEven, nil
	default:
		return 0, fmt.Errorf("非法 --parity %q（可选 none|odd|even）", s)
	}
}

func parseFlow(s string) (uint8, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "0":
		return wsp.FlowNone, nil
	case "rtscts", "rts/cts", "rts-cts", "hardware", "1":
		return wsp.FlowRTSCTS, nil
	default:
		return 0, fmt.Errorf("非法 --flow %q（可选 none|rtscts）", s)
	}
}

// config converts the parsed flags into a bridge configuration.
func (cf *commonFlags) config() (bridge.Config, error) {
	var cfg bridge.Config
	logger, err := cf.logger()
	if err != nil {
		return cfg, err
	}
	if cf.port < 1 || cf.port > 65535 {
		return cfg, fmt.Errorf("非法端口 %d（允许 1..65535）", cf.port)
	}
	if cf.baud != 0 && (cf.baud < 50 || cf.baud > 5000000) {
		return cfg, fmt.Errorf("非法波特率 %d（0 或 50..5000000）", cf.baud)
	}
	if cf.dataBits < 5 || cf.dataBits > 8 {
		return cfg, fmt.Errorf("非法数据位 %d（允许 5..8）", cf.dataBits)
	}
	if cf.stopBits != 1 && cf.stopBits != 2 {
		return cfg, fmt.Errorf("非法停止位 %d（允许 1 或 2）", cf.stopBits)
	}
	if cf.pingInterval < 0 {
		return cfg, fmt.Errorf("--ping-interval 不能为负数")
	}
	parity, err := parseParity(cf.parity)
	if err != nil {
		return cfg, err
	}
	flow, err := parseFlow(cf.flow)
	if err != nil {
		return cfg, err
	}

	cfg = bridge.Config{
		Host:         cf.host,
		Port:         cf.port,
		PortSet:      cf.portSet,
		Raw:          cf.raw,
		Device:       cf.device,
		Baud:         uint32(cf.baud),
		DataBits:     uint8(cf.dataBits),
		Parity:       parity,
		StopBits:     uint8(cf.stopBits),
		Flow:         flow,
		ResetOnStart: cf.resetOnStart,
		NoReconnect:  cf.noReconnect,
		PingInterval: cf.pingInterval,
		Logger:       logger,
	}
	if cfg.Baud == 0 && (cf.explicitFlags["databits"] || cf.explicitFlags["parity"] ||
		cf.explicitFlags["stopbits"] || cf.explicitFlags["flow"]) {
		logger.Warnf("--baud 0 表示保持设备当前串口配置，--databits/--parity/--stopbits/--flow 将被忽略")
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cf := &commonFlags{}
	addCommonFlags(fs, cf)
	fs.Usage = func() { printRunHelp(os.Stdout) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "wserial run: %v\n\n", err)
		printRunHelp(os.Stderr)
		return exitUsage
	}
	cf.noteExplicit(fs)

	if runtime.GOOS == "windows" && cf.device == "" {
		fmt.Fprintln(os.Stderr, "wserial run: Windows 下必须用 -d/--device 指定 com0com 的一端，例如：")
		fmt.Fprintln(os.Stderr, "  wserial run -d COM12 -H 192.168.1.50")
		fmt.Fprintln(os.Stderr, "然后用串口程序打开对端（如 COM11）。")
		return exitUsage
	}

	cfg, err := cf.config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wserial run: %v\n", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = bridge.Run(ctx, cfg)
	if err != nil {
		if errors.Is(err, bridge.ErrNoDevice) || errors.Is(err, bridge.ErrDeviceBusy) {
			cfg.Logger.Errorf("%v", err)
			return exitNoDevice
		}
		cfg.Logger.Errorf("%v", err)
		return exitError
	}
	cfg.Logger.Infof("已退出，虚拟串口已清理")
	return exitOK
}

// ---------------------------------------------------------------------------
// monitor
// ---------------------------------------------------------------------------

func cmdMonitor(args []string) int {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cf := &commonFlags{}
	addCommonFlags(fs, cf)
	var hexMode bool
	fs.BoolVar(&hexMode, "hex", false, "以十六进制显示收发数据")
	fs.Usage = func() { printMonitorHelp(os.Stdout) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "wserial monitor: %v\n\n", err)
		printMonitorHelp(os.Stderr)
		return exitUsage
	}
	cf.noteExplicit(fs)
	if cf.explicitFlags["d"] || cf.explicitFlags["device"] {
		fmt.Fprintln(os.Stderr, "提示: monitor 模式不创建虚拟串口，--device 被忽略")
	}

	cfg, err := cf.config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wserial monitor: %v\n", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	terminal := term.IsTerminal(int(os.Stdin.Fd()))
	if !terminal {
		cfg.Logger.Warnf("标准输入不是终端：按管道模式接收输入（Ctrl+] 不可用，Ctrl+C 退出）")
	}

	err = bridge.Monitor(ctx, bridge.MonitorConfig{
		Config:   cfg,
		Hex:      hexMode,
		In:       os.Stdin,
		Out:      os.Stdout,
		Terminal: terminal,
	})
	if err != nil {
		if errors.Is(err, bridge.ErrNoDevice) || errors.Is(err, bridge.ErrDeviceBusy) {
			cfg.Logger.Errorf("%v", err)
			return exitNoDevice
		}
		cfg.Logger.Errorf("%v", err)
		return exitError
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// help
// ---------------------------------------------------------------------------

func printGlobalUsage(w io.Writer) {
	fmt.Fprint(w, `wserial — 无线串口主机端工具（WSP/1）

把 ESP32-C3 通过 WiFi 暴露的串口变成本机虚拟串口或终端控制台。

用法:
  wserial <command> [选项]

命令:
  list       通过 mDNS 列出局域网内的无线串口设备
  run        创建虚拟串口并桥接到设备（Unix 默认 /tmp/ttyWS0）
  monitor    在当前终端直接收发串口数据（不创建虚拟串口）
  version    显示版本与协议信息
  help       显示本帮助

常用示例:
  wserial list                              # 发现设备
  wserial run                               # 自动发现 + /tmp/ttyWS0
  wserial run -H 192.168.1.50               # 直连设备
  wserial run -H 192.168.1.50 -b 9600       # 同时修改目标波特率
  wserial run --raw -H 192.168.1.50         # RAW 兼容模式（端口 3334）
  wserial monitor -H 192.168.1.50 --hex     # 终端监视（十六进制）

退出码:
  0  成功
  1  运行错误（连接失败、协议错误等）
  2  命令行用法错误
  4  未发现设备 / 设备被占用

提示: 每个子命令都支持 --help，例如 wserial run --help
`)
}

func printListHelp(w io.Writer) {
	fmt.Fprintf(w, `list — 通过 mDNS 发现 _wserial._tcp 设备

用法:
  wserial list [选项]

选项:
  --timeout <duration>   搜索时长（默认 3s）
  --json                 以 JSON 数组输出（便于脚本处理）
  -h, --help             显示本帮助

输出列: 名称、IP、端口、MAC、波特率
退出码: 0 成功；4 未发现设备
`)
}

func printRunHelp(w io.Writer) {
	devPath := vport.DefaultDevicePath()
	devDesc := "（Windows 必填，如 COM12）"
	if devPath != "" {
		devDesc = fmt.Sprintf("（默认 %s）", devPath)
	}
	fmt.Fprintf(w, `run — 创建虚拟串口并桥接 ESP32 的 WSP/RAW TCP 端口

用法:
  wserial run [选项]

连接:
  -H, --host <host>        设备 IP/主机名；留空则通过 mDNS 自动发现
  -P, --port <port>        TCP 端口（默认 %d；--raw 时默认 %d）
  --raw                    使用 RAW 模式（端口 %d，纯字节透传，无握手/无心跳）
  --no-reconnect           TCP 断开后不自动重连（默认指数退避 1s→10s 自动重连）
  --reset-on-start         连接后向目标发送 120ms 复位脉冲（RESET_TARGET）

虚拟串口:
  -d, --device <path>      虚拟串口路径%s
                           macOS/Linux 下创建 PTY 并符号链接到此路径；
                           Windows 下打开 com0com 端口对的一端（如 COM12），
                           用户程序打开对端（如 COM11）
  -b, --baud <n>           目标波特率（默认 115200；0 = 保持设备当前值，不下发配置）
  --databits <5-8>         数据位（默认 8）
  --parity <none|odd|even> 校验位（默认 none）
  --stopbits <1|2>         停止位（默认 1）
  --flow <none|rtscts>     硬件流控（默认 none）

会话:
  --ping-interval <dur>    WSP 心跳间隔（默认 15s；0 = 关闭）
  -v, --verbose            等价于 --log-level debug
  --log-level <level>      日志级别 error|warn|info|debug|trace（默认 info，全部输出到 stderr）
  -h, --help               显示本帮助

示例:
  wserial run                               # mDNS 发现，创建 %s
  wserial run -H 192.168.1.50               # 直连设备
  wserial run -b 9600 -d /tmp/ttyWS1        # 修改波特率与符号链接路径
  wserial run --raw -H 192.168.1.50         # 兼容模式，纯 TCP 透传

macOS/Linux 用法示例:
  wserial run &
  screen %s 115200        # 或 picocom / minicom / 自己的程序
  say "hello" > %s        # 也可以直接用文件 I/O

按 Ctrl+C 退出；退出时会删除符号链接。
`, wsp.DefaultWSPPort, wsp.DefaultRAWPort, wsp.DefaultRAWPort, devDesc, devPath, devPath, devPath)
}

func printMonitorHelp(w io.Writer) {
	fmt.Fprintf(w, `monitor — 在当前终端直接显示串口收发（不创建虚拟串口）

用法:
  wserial monitor [选项]

选项:
  -H, --host <host>        设备 IP/主机名；留空则通过 mDNS 自动发现
  -P, --port <port>        TCP 端口（默认 %d；--raw 时默认 %d）
  --raw                    RAW 模式（端口 %d，纯字节透传，无握手）
  --hex                    以十六进制显示收发数据（带 RX/TX 前缀）
  --no-reconnect           断开后不自动重连
  --reset-on-start         连接后向目标发送 120ms 复位脉冲
  -b, --baud <n>           目标波特率（默认 115200；0 = 保持设备当前值）
  --databits <5-8>         数据位（默认 8）
  --parity <none|odd|even> 校验位（默认 none）
  --stopbits <1|2>         停止位（默认 1）
  --flow <none|rtscts>     硬件流控（默认 none）
  --ping-interval <dur>    WSP 心跳间隔（默认 15s；0 = 关闭）
  -d, --device <path>      仅为 CLI 一致性保留；monitor 不使用虚拟串口
  -v, --verbose            等价于 --log-level debug
  --log-level <level>      日志级别 error|warn|info|debug|trace（默认 info）
  -h, --help               显示本帮助

退出:
  Ctrl+] 退出（终端原始模式）；Ctrl+C 亦可
  管道输入时 Ctrl+] 不生效，Ctrl+C 退出

示例:
  wserial monitor -H 192.168.1.50
  wserial monitor -H 192.168.1.50 --hex
  wserial monitor --raw -H 192.168.1.50
`, wsp.DefaultWSPPort, wsp.DefaultRAWPort, wsp.DefaultRAWPort)
}
