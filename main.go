// remote-go: a UU-Remote-style remote desktop with no server of its own.
//
//	remote-go                          the desktop app (NSGo native UI)
//	remote-go host                     controlled side, prints device code + verification code
//	remote-go view <device code> -p X  controller: open the remote desktop in a local browser
//
// Devices connect through tailcat (Tailscale's WireGuard data plane and
// DERP relays, without accounts or a control plane). The host's tunnel key
// is derived from its device code and verification code.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tailcfg"

	"github.com/i5shuyi/remote-go/internal/gui"
	"github.com/i5shuyi/remote-go/internal/host"
	"github.com/i5shuyi/remote-go/internal/liveupdate"
	"github.com/i5shuyi/remote-go/internal/mobile"
	"github.com/i5shuyi/remote-go/internal/tunnel"
	"github.com/i5shuyi/remote-go/internal/viewer"
)

func usage() {
	fmt.Fprint(os.Stderr, `remote-go — 基于 tailcat 的无服务器远程桌面

用法:
  remote-go                                打开桌面客户端（图形界面）
  remote-go host  [选项]                   作为被控端运行（显示设备代码和验证码）
  remote-go view  [选项] -p <验证码> <设备代码>  连接被控端，并在本机浏览器中打开控制界面
  remote-go local [选项]                   不建隧道，仅在本机 127.0.0.1 上测试被控端

两端经 tailcat 建立 WireGuard 隧道（先走 DERP 中继，再自动打洞直连），不需要任何账号。
用 "remote-go <命令> -h" 查看各命令的选项。
`)
}

func main() {
	log.SetFlags(log.Ltime)
	// Android has only the phone app, a controller; RG_MOBILE=1 opens it
	// on a desktop, for development.
	if runtime.GOOS == "android" || os.Getenv("RG_MOBILE") != "" {
		if err := mobile.Run(); err != nil {
			log.Fatal(err)
		}
		return
	}
	// No arguments (or launched from Finder/Explorer): open the desktop app.
	if len(os.Args) < 2 || strings.HasPrefix(os.Args[1], "-psn_") {
		if err := gui.Run(); err != nil {
			log.Fatal(err)
		}
		return
	}
	// A session window is an independent GUI process with normal OS exit signals.
	if os.Args[1] == "session-window" {
		if err := gui.SessionWindow(); err != nil {
			log.Fatal(err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "session-host":
		err = liveupdate.Serve(ctx)
	case "host":
		err = runHost(ctx, args, false)
	case "local":
		err = runHost(ctx, args, true)
	case "view":
		err = runView(ctx, args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func runHost(ctx context.Context, args []string, localOnly bool) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	name, _ := os.Hostname()
	nameFlag := fs.String("name", strings.TrimSuffix(name, ".local"), "控制端看到的设备名")
	device := fs.String("device", os.Getenv("RG_DEVICE"), "固定设备代码（9 位，默认按最近的中继新生成）")
	pass := fs.String("p", os.Getenv("RG_PASSWORD"), "固定验证码（默认随机生成 8 位）")
	display := fs.Int("display", 0, "要共享的显示器编号")
	viewOnly := fs.Bool("view-only", false, "仅允许观看，不接受键鼠控制")
	addr := fs.String("addr", "127.0.0.1:7900", "local 模式下的监听地址")
	verbose := fs.Bool("v", false, "输出 tailcat 调试日志")
	var relay string
	var region int
	if !localOnly {
		fs.StringVar(&relay, "derp", os.Getenv("RG_DERP"), "自建 DERP 域名或 host:port")
		fs.IntVar(&region, "region", 0, "指定公共中继区域（301–309，0 自动选择）")
	}
	fs.Parse(args)

	var code string
	if !localOnly {
		var err error
		code, err = hostCode(ctx, *device, region, relay)
		if err != nil {
			return err
		}
	}
	cfg := host.Config{Name: *nameFlag, Code: *pass, Display: *display, ViewOnly: *viewOnly}
	if cfg.Code == "" {
		cfg.Code = host.RandomCode()
	}
	h, err := host.New(cfg)
	if err != nil {
		return err
	}
	if localOnly {
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return err
		}
		banner("本机测试", h.Code(), "http://"+ln.Addr().String()+"/")
		go func() { <-ctx.Done(); ln.Close() }()
		return h.Serve(ln)
	}

	ln, err := tunnel.Listen(ctx, code, h.Code(), relay, tunnel.Logf(*verbose))
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	viewCmd := "remote-go view"
	if endpoint, _ := tunnel.NormalizeRelay(relay); endpoint != "" {
		viewCmd += " -derp " + endpoint
	}
	banner(code, h.Code(), fmt.Sprintf("%s -p %s %s", viewCmd, h.Code(), code))
	return h.Serve(ln)
}

func hostCode(ctx context.Context, device string, region int, relay string) (string, error) {
	code := tunnel.Normalize(device)
	endpoint, err := tunnel.NormalizeRelay(relay)
	if err != nil {
		return "", err
	}
	if region != 0 && (region < 301 || region > 309) {
		return "", fmt.Errorf("公共中继区域应为 301 至 309")
	}
	if endpoint != "" {
		if region != 0 {
			return "", fmt.Errorf("-derp 和 -region 请选择一个")
		}
		region = tunnel.CustomRegion
	}
	if code != "" {
		id, err := tunnel.Region(code)
		if err != nil {
			return "", err
		}
		if id == tunnel.CustomRegion && endpoint == "" {
			return "", fmt.Errorf("末位为 0 的设备代码需要 -derp 地址")
		}
		if region != 0 && int(id) != region {
			return "", fmt.Errorf("固定设备代码的末位与指定中继不符")
		}
		return code, nil
	}
	id := tailcfg.DERPRegionID(region)
	if id == 0 {
		probe, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		id, err = tunnel.NearestRegion(probe)
		if err != nil {
			return "", err
		}
	}
	return tunnel.NewDeviceCode(id)
}

func banner(code, pass, how string) {
	fmt.Printf("\n  ┌──────────────────────────────────────────\n")
	fmt.Printf("  │ 设备代码  %s\n", code)
	fmt.Printf("  │ 验证码    %s\n", pass)
	fmt.Printf("  │ 连接      %s\n", how)
	fmt.Printf("  └──────────────────────────────────────────\n\n")
}

func runView(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("view", flag.ExitOnError)
	pass := fs.String("p", os.Getenv("RG_PASSWORD"), "对方的验证码")
	listen := fs.String("listen", "127.0.0.1:0", "本地浏览器访问地址")
	relay := fs.String("derp", os.Getenv("RG_DERP"), "连接末位为 0 的设备所用的自建 DERP 地址")
	noOpen := fs.Bool("no-open", false, "不自动打开浏览器")
	verbose := fs.Bool("v", false, "输出 tailcat 调试日志")
	fs.Parse(args)
	if fs.NArg() != 1 || *pass == "" {
		return fmt.Errorf("用法: remote-go view -p <验证码> <设备代码>")
	}
	log.Printf("正在建立 tailcat 隧道…")
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	tc, err := tunnel.Dial(dctx, fs.Arg(0), *pass, *relay, tunnel.Logf(*verbose))
	cancel()
	if err != nil {
		return err
	}
	defer tc.Close()
	go func() {
		time.Sleep(3 * time.Second)
		if p := tc.Path(ctx); p != "" {
			log.Printf("隧道路径：%s", p)
		}
	}()
	return viewer.Run(ctx, tc.DialContext, "remote-go", tunnel.Port, *listen, !*noOpen)
}
