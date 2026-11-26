// cmd/catalogprobe 上游目录 / 端点取证工具（只读诊断，不进用户链路）。
//
// 一条命令回答三件事：
//  1. 目录端点还活不活（/v3/config、/console/enterprises/personal/models）；
//  2. 每路 UA 各下发哪些模型（官方桌面 / 历史 IDE / 历史 CLI，三路集合本就不同）；
//  3. 上游到底给这个账号标了多少 disabled（下架 / 该账号类型不可用）。
//
// 只发 GET，不发推理请求，不消费积分；报告里不出现 access_token / refresh_token，
// 账号只有 UID 的短 hash。`-public` 会额外发 3 次**无鉴权**请求判断路径在不在。
//
// 用法（在网关机器上、与 auths/ 同机执行）：
//
//	go run ./cmd/catalogprobe -auth-dir ./auths
//	go run ./cmd/catalogprobe -auth-dir ./auths -realm global -json
//	go run ./cmd/catalogprobe -auth-dir ./auths -baseline catalog-cn.json
//	go run ./cmd/catalogprobe -auth-dir ./auths -write-baseline catalog-cn.json
//
// 与 cmd/hdrprobe 同定位：排障工具，不随 Release 分发包走。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/modelid"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "账号目录（workbuddy-*.json）")
	uid := flag.String("uid", "", "指定账号 UID；缺省按 -realm 取第一个可用账号")
	realm := flag.String("realm", "", "cn / global；缺省优先 cn")
	asJSON := flag.Bool("json", false, "输出 JSON（可存档、可手工 diff）")
	showModels := flag.Bool("models", false, "额外打印每路可用模型 id 全量")
	public := flag.Bool("public", false, "额外做 3 次无鉴权探测（只看路径在不在）")
	baseline := flag.String("baseline", "", "与既有指纹文件对比（只读）")
	writeBase := flag.String("write-baseline", "", "把本次指纹写到文件（0600）")
	flag.Parse()

	accts, err := auth.LoadDir(*authDir)
	if err != nil {
		fail("读取账号目录 %s: %v", *authDir, err)
	}
	a := pickAccount(accts, *uid, *realm)
	if a == nil {
		fail("账号目录 %s 里没有可用账号（-uid=%q -realm=%q）", *authDir, *uid, *realm)
	}

	c := upstream.New()
	rep := c.ProbeCatalog(a)
	if *public {
		rep.Public = c.ProbePublicEndpoints(rep.ChatBase, upstream.DefaultPublicProbePaths)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fail("输出 JSON: %v", err)
		}
	} else {
		printReport(rep, *showModels)
	}

	if *writeBase != "" {
		fp := rep.Fingerprint()
		raw, err := json.MarshalIndent(fp, "", " ")
		if err != nil {
			fail("序列化指纹: %v", err)
		}
		if err := os.WriteFile(*writeBase, raw, 0o600); err != nil {
			fail("写指纹 %s: %v", *writeBase, err)
		}
		fmt.Printf("\n指纹已写入 %s\n", *writeBase)
	}

	if *baseline != "" {
		old, err := readFingerprint(*baseline)
		if err != nil {
			fail("读取指纹 %s: %v", *baseline, err)
		}
		diff := upstream.DiffFingerprint(old, rep.Fingerprint())
		if len(diff) == 0 {
			fmt.Printf("\n与 %s 对比：没有变化\n", *baseline)
		} else {
			fmt.Printf("\n与 %s 对比（%d 项）：\n", *baseline, len(diff))
			for _, line := range diff {
				fmt.Println("  " + line)
			}
		}
	}
}

// pickAccount 按 -uid / -realm 选一个账号：uid 精确匹配优先；否则 realm 过滤后取第一个
// 有 access_token 的；都没写 realm 时优先 cn（生产目录以 CN 为主）。
func pickAccount(accts []*auth.Auth, uid, realm string) *auth.Auth {
	var fallback *auth.Auth
	for _, a := range accts {
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if uid != "" && a.UID == uid {
			return a
		}
		if realm != "" && a.Realm() != realm {
			continue
		}
		if fallback == nil {
			fallback = a
		}
		if realm == "" && a.Realm() == modelid.RealmCN {
			return a
		}
	}
	return fallback
}

// printReport 人读版输出：先一行概览，再逐路一行，最后 disabled 明细。
func printReport(rep upstream.CatalogProbeReport, showModels bool) {
	fmt.Printf("realm=%s account=%s base=%s probed_at=%s\n",
		rep.Realm, rep.AccountTag, rep.ChatBase, rep.ProbedAt)
	for _, r := range rep.Routes {
		head := fmt.Sprintf("%-28s %3d  total=%-3d enabled=%-3d disabled=%-3d",
			r.Label+" "+r.Path, r.Status, r.Total, r.Enabled, len(r.Disabled))
		if r.Err != "" {
			head += "  err=" + short(r.Err, 120)
		}
		fmt.Println(head)
		if len(r.EnvelopeKeys) > 0 {
			fmt.Println("    envelope: " + strings.Join(r.EnvelopeKeys, ", "))
		}
		if showModels && len(r.Models) > 0 {
			ids := append([]string(nil), r.Models...)
			sort.Strings(ids)
			fmt.Println("    models: " + strings.Join(ids, ", "))
		}
	}
	var dis []string
	for _, r := range rep.Routes {
		for _, id := range r.Disabled {
			dis = append(dis, r.Label+":"+id)
		}
	}
	if len(dis) > 0 {
		sort.Strings(dis)
		fmt.Println("disabled 明细: " + strings.Join(dis, ", "))
	}
	for _, p := range rep.Public {
		line := fmt.Sprintf("public %-42s %3d", p.Path, p.Status)
		if p.ContentType != "" {
			line += "  " + p.ContentType
		}
		if p.Err != "" {
			line += "  err=" + short(p.Err, 100)
		}
		fmt.Println(line)
	}
}

// readFingerprint 读上一次的指纹文件（容错：坏文件直接报错，不猜）。
func readFingerprint(path string) (upstream.CatalogFingerprint, error) {
	var fp upstream.CatalogFingerprint
	raw, err := os.ReadFile(path)
	if err != nil {
		return fp, err
	}
	if err := json.Unmarshal(raw, &fp); err != nil {
		return fp, fmt.Errorf("parse: %w", err)
	}
	return fp, nil
}

// short 截断长字符串（错误体里常带上游 HTML 头几行）。
func short(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
