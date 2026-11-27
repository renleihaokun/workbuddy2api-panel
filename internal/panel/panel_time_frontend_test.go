package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// panelTimeFields 面板上「可能是 Go 零值 time.Time」的字段。
//
// Go 的 time.Time 是结构体，`omitempty` 对它无效（encoding/json 的 isEmptyValue 对
// struct 直接返回 false），所以「本进程从未拉过」序列化出来是 "0001-01-01T00:00:00Z"，
// 不是 null、也不是省略。这个字符串在 JS 里是**真值**，Date.parse 给它
// -62135596800000（**不是 NaN**），一路走到渲染层就变成假数据：
//
//   - fetched_at：任务中心把「还没扫过」渲染成「账号池是空的」，模型页与券码页因 ago(number) 抛 TypeError。
//   - balance_refreshed_at：到期卡片印出「1065522846 分钟前更新」。
//   - last_sample_at：积分走势脚注印出 0001 年。
//
// 约束：这些字段的每一处读取都必须先经 parseAPITime 归一化（零值 → 0）。
var panelTimeFields = []string{"fetched_at", "balance_refreshed_at", "last_sample_at"}

// stripJSComments 剥掉 // 行注释与 /* */ 块注释（只服务于源码扫描：app.js 里没有
// 在字符串字面量中出现 // 或 /* 的场景）。保留换行，报错时仍能看出结构。
func stripJSComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inBlock := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inBlock {
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
				continue
			}
			if c == '\n' {
				b.WriteByte('\n')
			}
			continue
		}
		if c == '/' && i+1 < len(src) {
			switch src[i+1] {
			case '*':
				inBlock = true
				i++
				continue
			case '/':
				for i < len(src) && src[i] != '\n' {
					i++
				}
				if i < len(src) {
					b.WriteByte('\n')
				}
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// collapseJS 把连续空白压成一个空格：判定按语句窗口切，不关心缩进与换行。
func collapseJS(code string) string { return strings.Join(strings.Fields(code), " ") }

// jsStatementAt 取 idx 所在语句的文本：向上找最近的 ; { }，向下找下一个 ;。
// app.js 的语句都以 ; 收尾，块级作用域由 { } 兜住，够用。
func jsStatementAt(code string, idx int) string {
	start := strings.LastIndexAny(code[:idx], ";{}") + 1
	end := strings.Index(code[idx:], ";")
	if end < 0 {
		end = len(code) - idx
	}
	stmt := strings.TrimSpace(code[start : idx+end])
	if r := []rune(stmt); len(r) > 200 {
		stmt = string(r[:200]) + "…"
	}
	return stmt
}

// TestAppJSTimeFieldsUseParseAPITime 钉住上面的约束，防止「读接口零上游」这类改动
// 再次把零值时间喂给渲染层。
//
// 判定按「语句」而不是「物理行」：先把注释整体剥掉（注释里提到字段名不算读取），
// 再压平空白按 ; { } 切语句窗口。这样正确写法即使换行成
// `parseAPITime(\n  d.fetched_at\n)` 也不会误报，而行尾注释里顺手补一句
// `// 本该走 parseAPITime(` 也不能蒙混过关。两条误报/漏报都是实测踩出来的。
func TestAppJSTimeFieldsUseParseAPITime(t *testing.T) {
	b, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	code := collapseJS(stripJSComments(string(b)))
	// 风格约束：ago() 直接接受 ISO 字符串 / 毫秒数 / Date，外面别再包一层
	// new Date(..).getTime()——1.30.0 的模型页与券码页正是被这一层包装传成 number
	// 才抛 TypeError 的。
	if strings.Contains(code, "ago(new Date(") {
		t.Errorf("app.js must not call ago(new Date(...))；ago 直接接受原始值")
	}
	for _, f := range panelTimeFields {
		for from := 0; ; {
			i := strings.Index(code[from:], f)
			if i < 0 {
				break
			}
			i += from
			if stmt := jsStatementAt(code, i); !strings.Contains(stmt, "parseAPITime(") {
				t.Errorf("app.js 有一处 %s 未经 parseAPITime 归一化：%s", f, stmt)
			}
			from = i + len(f)
		}
	}
}

// TestAppJSAgoAcceptsNumbers ago() 必须同时接受 ISO 字符串 / 毫秒数 / Date，
// 零时间与空值返回「—」（此前传数字会抛 iso.startsWith is not a function）。
func TestAppJSAgoAcceptsNumbers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; ago test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function ago(');
const e = src.indexOf('function dur(');
if (s < 0 || e <= s) throw new Error('ago not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(s, e) + '\nthis.ago = ago;', ctx);
const ago = ctx.ago;
const t2 = Date.now() - 120000;
const cases = [
  [ago(t2), '2 分钟前'],
  [ago(new Date(t2)), '2 分钟前'],
  [ago(new Date(t2).toISOString()), '2 分钟前'],
  [ago('0001-01-01T00:00:00Z'), '—'],
  [ago(0), '—'],
  [ago(''), '—'],
  [ago(null), '—'],
  [ago('garbage'), '—'],
];
let bad = 0;
for (const [got, want] of cases) { if (got !== want) { bad++; console.log('got', JSON.stringify(got), 'want', JSON.stringify(want)); } }
process.exit(bad ? 1 : 0);
`
	f, err := os.CreateTemp(t.TempDir(), "ago-*.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("ago test failed: %v\n%s", err, out)
	}
}

// TestAppJSFmtStampShortRejectsZeroTime 表格/日志里的时间戳格式化必须把 Go 零值时间
// 显示成「—」，而不是「01-01 07:36」（本机时区下零值 time 的模样）——后者看着像
// 一条真实记录，排查时会被当成「真的有数据」。
func TestAppJSFmtStampShortRejectsZeroTime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; fmtStampShort test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function fmtStampShort');
const e = src.indexOf('function dtLocalValue');
if (s < 0 || e <= s) throw new Error('fmtStampShort not found');
const ctx = { Date, Number, String, Math, isNaN };
vm.createContext(ctx);
vm.runInContext(src.slice(s, e) + '\nthis.f = fmtStampShort;', ctx);
const f = ctx.f;
const out = {
  zeroString: f('0001-01-01T00:00:00Z'),
  zeroParsed: f(0),
  empty: f(''),
  nullish: f(null),
  garbage: f('garbage'),
  real: f('2026-11-21T12:00:00Z'),
};
process.stdout.write(JSON.stringify(out));
`
	f, err := os.CreateTemp(t.TempDir(), "stamp-*.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("fmtStampShort test failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	var res struct {
		ZeroString string `json:"zeroString"`
		ZeroParsed string `json:"zeroParsed"`
		Empty      string `json:"empty"`
		Nullish    string `json:"nullish"`
		Garbage    string `json:"garbage"`
		Real       string `json:"real"`
	}
	if err := json.Unmarshal([]byte(got), &res); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	for name, v := range map[string]string{
		"零时间字符串": res.ZeroString, "毫秒 0": res.ZeroParsed,
		"空串": res.Empty, "null": res.Nullish, "垃圾串": res.Garbage,
	} {
		if v != "—" {
			t.Errorf("fmtStampShort(%s)=%q want —", name, v)
		}
	}
	if res.Real == "—" || !strings.Contains(res.Real, "11-21") {
		t.Errorf("fmtStampShort(真实时间)=%q，应当正常渲染 MM-DD HH:mm:ss", res.Real)
	}
}
