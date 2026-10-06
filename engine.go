package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const createNewProcessGroup = 0x00000200

// AccountInfo 是单个账号的展示用快照。
//
// 上游 /status 的字段并不保证齐全（早期另一个引擎只有 uid / nickname / credits），
// 所以解析时缺字段留零值，由前端按“-”显示。
type AccountInfo struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Credits   int    `json:"credits"`
	CreditCap int    `json:"credit_cap"` // 总额度；上游未上报时为 0
	Expiry    string `json:"expiry"`     // 最早过期时间
	Expiring  int    `json:"expiring"`   // 即将过期的额度
	Realm     string `json:"realm"`      // cn / global
	Cooling   bool   `json:"cooling"`
	Disabled  bool   `json:"disabled"`
}

// EngineSnapshot 是引擎 /status 的结构化解析结果。
type EngineSnapshot struct {
	Online       bool          `json:"-"`
	Total        int           `json:"-"`
	Healthy      int           `json:"-"`
	Cooling      int           `json:"-"`
	Disabled     int           `json:"-"`
	Credits      int           `json:"-"` // 剩余积分合计
	CreditCap    int           `json:"-"` // 总额度合计（上游无此字段时退化为 Credits）
	Accounts     []AccountInfo `json:"-"`
	CreditsKnown bool          `json:"-"` // 是否有任一账号上报了额度（用于区分「0 分」与「未取到」）
}

// Engine 表示一个账号池引擎（wb2api）的运行时状态与进程句柄。
type Engine struct {
	Key        string // wb
	Title      string // 展示名
	Dir        string // 工作目录（config.json / auths / data 所在）
	Exe        string // 可执行文件绝对路径
	ListenAddr string // 127.0.0.1:port
	APIKey     string // 客户端 Bearer 鉴权密钥
	EnvKeyName string // WB2A_API_KEY
	EnvListen  string // WB2A_LISTEN

	mu        sync.Mutex
	cmd       *exec.Cmd
	pid       int
	startedAt time.Time // 本进程拉起该引擎的时刻（外部实例为零值）
	logPath   string
}

// PID 返回本进程拉起的引擎 PID（0 = 非本进程拉起）。
func (e *Engine) PID() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pid
}

// StartedAt 返回本进程拉起该引擎的时刻（外部实例为零值）。
func (e *Engine) StartedAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startedAt
}

// Port 返回监听端口。
func (e *Engine) Port() int {
	_, p, err := net.SplitHostPort(e.ListenAddr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

func (e *Engine) baseURL() string { return "http://" + e.ListenAddr }

// running 判断引擎是否在跑：本进程拉起的 PID 存活，或端口已被监听（外部实例）。
func (e *Engine) running() bool {
	e.mu.Lock()
	pid := e.pid
	e.mu.Unlock()
	if pid != 0 {
		return true
	}
	return portInUse(e.ListenAddr)
}

// start 拉起引擎（幂等）。已运行则直接返回。
func (e *Engine) start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pid != 0 || portInUse(e.ListenAddr) {
		return nil
	}
	if _, err := os.Stat(e.Exe); err != nil {
		return fmt.Errorf("%s 引擎文件缺失: %s", e.Title, e.Exe)
	}
	_ = os.MkdirAll(e.Dir, 0o755)

	logf, _ := os.OpenFile(e.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd := exec.Command(e.Exe, "-config", "config.json")
	cmd.Dir = e.Dir
	cmd.Env = append(os.Environ(), e.EnvKeyName+"="+e.APIKey, e.EnvListen+"="+e.ListenAddr)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup}
	if logf != nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", e.Title, err)
	}
	e.cmd = cmd
	e.pid = cmd.Process.Pid
	e.startedAt = time.Now()
	pid := e.pid
	go func() {
		_ = cmd.Wait()
		e.mu.Lock()
		if e.pid == pid {
			e.pid = 0
			e.cmd = nil
			e.startedAt = time.Time{}
		}
		e.mu.Unlock()
	}()
	return nil
}

// stop 结束引擎：先杀本进程拉起的进程树，再兜底清理占用端口的监听进程。
func (e *Engine) stop() {
	e.mu.Lock()
	pid := e.pid
	e.mu.Unlock()
	if pid != 0 {
		killTree(pid)
	}
	if p := pidOnPort(e.ListenAddr); p != 0 && p != pid {
		killTree(p)
	}
}

// health 读取引擎 /status，返回是否在线与账号数（轻量，供高频轮询）。
func (e *Engine) health() (online bool, accounts int) {
	snap := e.fetch()
	return snap.Online, len(snap.Accounts)
}

// fetch 读取引擎 /status 并解析为结构化快照。
//
// 上游字段集不保证齐全（早期另一个引擎的响应体只有一个 accounts 数组），
// 因此汇总值缺字段时自行从 accounts 推导。
// 解析失败时返回 Online=false，由前端显示「接口不可达」而不是假装 0 账号。
func (e *Engine) fetch() EngineSnapshot {
	req, err := http.NewRequest("GET", e.baseURL()+"/status", nil)
	if err != nil {
		return EngineSnapshot{}
	}
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	cl := &http.Client{Timeout: 4 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return EngineSnapshot{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return EngineSnapshot{}
	}

	var v struct {
		Total    int `json:"total"`
		Healthy  int `json:"healthy"`
		Cooling  int `json:"cooling"`
		Disabled int `json:"disabled"`
		Accounts []struct {
			UID       string  `json:"uid"`
			Nickname  string  `json:"nickname"`
			Credits   float64 `json:"credits"`
			CreditCap float64 `json:"credits_total"`
			Expiry    string  `json:"credits_earliest_expiry"`
			Expiring  float64 `json:"credits_earliest_remaining"`
			Realm     string  `json:"realm"`
			Cooling   bool    `json:"cooling"`
			Disabled  bool    `json:"disabled"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return EngineSnapshot{}
	}

	s := EngineSnapshot{
		Online:   true,
		Total:    v.Total,
		Healthy:  v.Healthy,
		Cooling:  v.Cooling,
		Disabled: v.Disabled,
	}
	for _, a := range v.Accounts {
		info := AccountInfo{
			UID:       a.UID,
			Nickname:  a.Nickname,
			Credits:   int(a.Credits),
			CreditCap: int(a.CreditCap),
			Expiry:    a.Expiry,
			Expiring:  int(a.Expiring),
			Realm:     a.Realm,
			Cooling:   a.Cooling,
			Disabled:  a.Disabled,
		}
		if info.Nickname == "" {
			info.Nickname = shortUID(a.UID)
		}
		s.Accounts = append(s.Accounts, info)
		s.Credits += info.Credits
		s.CreditCap += info.CreditCap
		if info.CreditCap > 0 {
			s.CreditsKnown = true
		}
	}
	// 汇总字段缺失时自己推；没有额度字段时把额度记为未知。
	if s.Total == 0 {
		s.Total = len(s.Accounts)
	}
	if s.Healthy == 0 {
		for _, a := range s.Accounts {
			if !a.Disabled && !a.Cooling {
				s.Healthy++
			} else if a.Cooling {
				s.Cooling++
			} else {
				s.Disabled++
			}
		}
	}
	return s
}

// shortUID 在缺少昵称时生成可读的短标识。
func shortUID(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "未命名"
	}
	if len(uid) <= 10 {
		return uid
	}
	return uid[:8] + "…"
}

// ---------------------------------------------------------------- 通用工具

// portInUse 探测地址是否已被监听。
func portInUse(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 700*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// pidOnPort 从 netstat 反查监听指定端口的 PID。
func pidOnPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && strings.HasSuffix(f[1], ":"+port) && strings.EqualFold(f[3], "LISTENING") {
			if pid, e2 := strconv.Atoi(f[4]); e2 == nil && pid > 0 {
				return pid
			}
		}
	}
	return 0
}

// killTree 结束进程及其子进程（隐藏窗口）。
func killTree(pid int) {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// runCmd 在 dir 下执行命令，带超时，返回 UTF-8 化的合并输出与退出码。
func runCmd(dir string, timeout time.Duration, name string, args ...string) (string, int) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return toUTF8(out), 124
	}
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return toUTF8(out), code
}

// toUTF8 将引擎/工具输出统一为 UTF-8。
//
// 不能简单二分「合法 UTF-8 就是 UTF-8，否则当 GBK」：signin.exe 之类的工具
// 按**字节数**对齐列宽，会把中文昵称截成半个字符（实测 E5 B9 B4 → E5 B9 + 空格），
// 于是整段输出混入极少量非法序列。此时若整体按 GBK 解，正常的 UTF-8 中文全变乱码。
// 故策略：只有非法序列占比很低时按截断修补，占比高才判定为 GBK。
func toUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if len(b) > 0 {
		bad := 0
		for i := 0; i < len(b); {
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				bad++
			}
			i += size
		}
		// 5% 以下视为「少量截断」，保留 UTF-8 并丢掉残缺字节。
		if float64(bad)/float64(len(b)) < 0.05 {
			return strings.ToValidUTF8(string(b), "")
		}
	}
	if out, _, err := transform.String(simplifiedchinese.GBK.NewDecoder(), string(b)); err == nil {
		return out
	}
	return strings.ToValidUTF8(string(b), "")
}

// tailFile 读取文件末尾 n 行。
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := toUTF8(data)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
