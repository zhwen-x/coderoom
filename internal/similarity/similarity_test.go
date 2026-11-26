package similarity

import (
	"strings"
	"testing"
)

// The submission a student actually copies from, long enough to be worth
// comparing at all.
const bubbleCPP = `
#include <iostream>
using namespace std;

int main() {
    int n;
    cin >> n;
    int a[1005];
    for (int i = 0; i < n; i++) {
        cin >> a[i];
    }
    for (int i = 0; i < n - 1; i++) {
        for (int j = 0; j < n - 1 - i; j++) {
            if (a[j] > a[j + 1]) {
                int t = a[j];
                a[j] = a[j + 1];
                a[j + 1] = t;
            }
        }
    }
    for (int i = 0; i < n; i++) {
        cout << a[i];
        if (i < n - 1) cout << " ";
    }
    cout << endl;
    return 0;
}`

// The same program after the three things everyone tries: new names, new
// comments, new formatting.
const bubbleRenamed = `
#include <iostream>
using namespace std;

// 我自己写的冒泡排序
int main() {
    int count;        /* 数据个数 */
    cin >> count;
    int data[1005];
    for (int idx = 0; idx < count; idx++) { cin >> data[idx]; }

    for (int round = 0; round < count - 1; round++)
        for (int pos = 0; pos < count - 1 - round; pos++)
            if (data[pos] > data[pos + 1]) {
                int tmp = data[pos];
                data[pos] = data[pos + 1];
                data[pos + 1] = tmp;
            }

    for (int idx = 0; idx < count; idx++) {
        cout << data[idx];
        if (idx < count - 1) cout << " ";
    }
    cout << endl;
    return 0;
}`

// A different algorithm for the same problem, written independently.
const selectionCPP = `
#include <cstdio>
#include <algorithm>

int main() {
    int n;
    scanf("%d", &n);
    int v[1005];
    for (int i = 0; i < n; i++) scanf("%d", &v[i]);
    for (int i = 0; i < n; i++) {
        int best = i;
        for (int j = i + 1; j < n; j++) {
            if (v[j] < v[best]) best = j;
        }
        std::swap(v[i], v[best]);
    }
    for (int i = 0; i < n; i++) {
        printf("%d", v[i]);
        if (i + 1 < n) printf(" ");
    }
    printf("\n");
    return 0;
}`

func analyse(t *testing.T, id, lang, src string) Doc {
	t.Helper()
	d := Analyse(id, lang, src)
	if d.TooShort {
		t.Fatalf("%s: 只有 %d 个 token，测试样本应当足够长", id, d.Tokens)
	}
	return d
}

// The whole point of normalising the token stream: renaming every identifier
// and reflowing the braces is the first thing anyone does, and it must not
// help at all.
func TestRenamingAndReformattingDoesNotHide(t *testing.T) {
	a := analyse(t, "a", "cpp", bubbleCPP)
	b := analyse(t, "b", "cpp", bubbleRenamed)

	m := Compare(a, b)
	if m.Score < 80 {
		t.Errorf("改名+改注释+改排版后相似度只有 %d%%（共享 %d 个指纹），应当仍然很高",
			m.Score, m.Shared)
	}
}

// And the other half: two people who genuinely wrote different programs must
// not be accused of anything.
func TestDifferentAlgorithmsScoreLow(t *testing.T) {
	a := analyse(t, "a", "cpp", bubbleCPP)
	c := analyse(t, "c", "cpp", selectionCPP)

	m := Compare(a, c)
	if m.Score > 45 {
		t.Errorf("两种不同排序算法的相似度高达 %d%%（共享 %d 个指纹），误报太高",
			m.Score, m.Shared)
	}
}

func TestIdenticalSourceScoresFull(t *testing.T) {
	a := analyse(t, "a", "cpp", bubbleCPP)
	b := analyse(t, "b", "cpp", bubbleCPP)
	if m := Compare(a, b); m.Score != 100 {
		t.Errorf("完全相同的代码相似度为 %d%%，应为 100%%", m.Score)
	}
}

// A problem that leaves no room to differ makes everyone look guilty. Refusing
// to score those is the difference between a useful report and one a teacher
// learns to ignore.
func TestVeryShortSubmissionsAreNotScored(t *testing.T) {
	const tiny = `
#include <iostream>
int main(){int a,b;std::cin>>a>>b;std::cout<<a+b;return 0;}`

	d := Analyse("tiny", "cpp", tiny)
	if !d.TooShort {
		t.Fatalf("A+B 这种长度（%d token）应当被判为过短而跳过", d.Tokens)
	}
	if len(d.Fingerprints) != 0 {
		t.Error("过短的提交不应产生指纹")
	}
	// And comparing one against anything must be silent rather than wrong.
	full := analyse(t, "full", "cpp", bubbleCPP)
	if m := Compare(d, full); m.Score != 0 || m.Shared != 0 {
		t.Errorf("与过短提交的比较应当返回零分，得到 %+v", m)
	}
}

// Comments and whitespace carry no structure, so stripping them has to be
// total - otherwise a student adding a licence header shifts every hash.
func TestCommentsAndSpacingAreIgnored(t *testing.T) {
	plain := Tokenize("cpp", "int main(){return 0;}")
	noisy := Tokenize("cpp", `
		/* a long header comment
		   spanning several lines */
		int    main ( )   {        // trailing
		    return   0 ;
		}`)
	if strings.Join(plain, " ") != strings.Join(noisy, " ") {
		t.Errorf("注释与空白影响了 token 流:\n plain = %v\n noisy = %v", plain, noisy)
	}
}

func TestPythonCommentsAndStrings(t *testing.T) {
	toks := Tokenize("python", "x = 1  # 这是注释\nprint('hello')\n")
	joined := strings.Join(toks, " ")
	if strings.Contains(joined, "注释") {
		t.Errorf("Python 注释未被去除: %v", toks)
	}
	if !strings.Contains(joined, "STR") {
		t.Errorf("字符串应折叠为 STR: %v", toks)
	}
	if strings.Contains(joined, "hello") {
		t.Errorf("字符串内容不应进入 token 流: %v", toks)
	}
	if !strings.Contains(joined, "NUM") {
		t.Errorf("数字应折叠为 NUM: %v", toks)
	}
}

// Keywords have to survive, or every program collapses to the same stream of
// ID and punctuation and everything looks identical.
func TestKeywordsSurviveButNamesDoNot(t *testing.T) {
	toks := Tokenize("cpp", "while (counter < limit) { counter++; }")
	joined := strings.Join(toks, " ")
	if !strings.Contains(joined, "while") {
		t.Errorf("关键字 while 丢失: %v", toks)
	}
	if strings.Contains(joined, "counter") || strings.Contains(joined, "limit") {
		t.Errorf("标识符未归一化: %v", toks)
	}
	if !strings.Contains(joined, "++") {
		t.Errorf("多字符运算符被拆开了: %v", toks)
	}
}

// Malformed input is normal: students submit the wrong file, truncate a paste,
// or leave a quote open. None of it may panic.
func TestTokenizerSurvivesGarbage(t *testing.T) {
	for _, src := range []string{
		"", "\"unterminated", "'", "/* never closed", "\\", "////",
		"int x = \"a\\", "你好，世界", strings.Repeat("{", 5000),
		"'''not really python'''", "0x1p+3 1e10 .5f",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Tokenize(%q) panic: %v", src, r)
				}
			}()
			Tokenize("cpp", src)
			Tokenize("python", src)
			Analyse("x", "cpp", src)
		}()
	}
}

// The winnowing guarantee is the reason to sample at all: any shared passage
// of at least K+W-1 tokens must leave a shared fingerprint. Without it the
// sampling would be a coin flip and a copied function could slip through.
func TestWinnowingCatchesAnySharedPassage(t *testing.T) {
	// Two otherwise unrelated documents sharing one long run of tokens.
	shared := strings.Repeat("if ( ID < ID ) { ID = ID + NUM ; } ", 4)
	a := "int ID ( ) { " + shared + " return NUM ; }"
	b := "double ID ( ID , ID ) { while ( ID ) { } " + shared + " }"

	da := Analyse("a", "cpp", strings.ReplaceAll(a, "ID", "name"))
	db := Analyse("b", "cpp", strings.ReplaceAll(b, "ID", "other"))
	if da.TooShort || db.TooShort {
		t.Skip("构造的样本不够长")
	}
	if m := Compare(da, db); m.Shared == 0 {
		t.Errorf("共享了 %d 个 token 的长片段却没有共享指纹", len(Tokenize("cpp", shared)))
	}
}

// Inserting code at the top must not shift the fingerprints of everything
// after it - positional sampling is what distinguishes winnowing from simply
// keeping every n-th hash.
func TestFingerprintsAreStableUnderInsertion(t *testing.T) {
	base := analyse(t, "base", "cpp", bubbleCPP)
	withHeader := analyse(t, "hdr", "cpp",
		"#include <vector>\nint helper(int q){ return q * 2 + 1; }\n"+bubbleCPP)

	m := Compare(base, withHeader)
	if m.Score < 90 {
		t.Errorf("在前面插入一段代码后相似度降到 %d%%，指纹不够稳定", m.Score)
	}
}

func TestCompareIsSymmetric(t *testing.T) {
	a := analyse(t, "a", "cpp", bubbleCPP)
	b := analyse(t, "b", "cpp", bubbleRenamed)
	if x, y := Compare(a, b), Compare(b, a); x.Score != y.Score || x.Shared != y.Shared {
		t.Errorf("比较不对称: %+v vs %+v", x, y)
	}
}

func TestFingerprintsAreSortedAndUnique(t *testing.T) {
	d := analyse(t, "a", "cpp", bubbleCPP)
	for i := 1; i < len(d.Fingerprints); i++ {
		if d.Fingerprints[i] <= d.Fingerprints[i-1] {
			t.Fatalf("指纹未排序去重: 位置 %d", i)
		}
	}
}

// A sanity check on the sampling rate: keeping everything would work but
// wastes memory, keeping almost nothing would break the guarantee.
func TestWinnowingActuallySamples(t *testing.T) {
	d := analyse(t, "a", "cpp", bubbleCPP)
	grams := len(Tokenize("cpp", bubbleCPP)) - K + 1
	if len(d.Fingerprints) >= grams {
		t.Errorf("没有起到采样作用: %d 个指纹 / %d 个 k-gram", len(d.Fingerprints), grams)
	}
	if len(d.Fingerprints) < grams/W {
		t.Errorf("采样过于稀疏: %d 个指纹 / %d 个 k-gram", len(d.Fingerprints), grams)
	}
	t.Logf("%d 个 token -> %d 个指纹（%d 个 k-gram）", d.Tokens, len(d.Fingerprints), grams)
}

// Students submit from Windows machines, and the same program arriving with
// CRLF line endings must not look like a different one. The tokenizer treats
// \r as whitespace, but that is an easy thing to lose in a later edit and the
// failure would be invisible: two identical answers simply stop matching.
func TestLineEndingsDoNotAffectTheComparison(t *testing.T) {
	lf := bubbleCPP
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	cr := strings.ReplaceAll(lf, "\n", "\r")

	base := analyse(t, "lf", "cpp", lf)
	for name, variant := range map[string]string{"CRLF": crlf, "CR": cr} {
		d := analyse(t, name, "cpp", variant)
		if d.Tokens != base.Tokens {
			t.Errorf("%s 的 token 数为 %d，LF 为 %d", name, d.Tokens, base.Tokens)
		}
		if m := Compare(base, d); m.Score != 100 {
			t.Errorf("同一份代码换成 %s 行尾后相似度只有 %d%%", name, m.Score)
		}
	}
}

// Likewise a UTF-8 BOM, which Windows editors add silently when saving.
func TestLeadingBOMDoesNotAffectTheComparison(t *testing.T) {
	base := analyse(t, "plain", "cpp", bubbleCPP)
	withBOM := analyse(t, "bom", "cpp", "\ufeff"+bubbleCPP)
	if m := Compare(base, withBOM); m.Score != 100 {
		t.Errorf("带 BOM 的同一份代码相似度只有 %d%%", m.Score)
	}
}
