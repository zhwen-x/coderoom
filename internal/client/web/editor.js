'use strict';

// A dependency-free code editor: a transparent <textarea> over a highlighted
// <pre>. Written by hand rather than pulling in CodeMirror/Monaco because the
// client must be a single self-contained executable that works with no network
// and no install step.
//
// The paste hook matters beyond convenience. Students compose in the lab's own
// IDE and paste the result in here, so the clipboard cannot be blocked - which
// removes "disable paste" as an anti-plagiarism measure. Recording what was
// pasted, and when, is the replacement.

(function (global) {

  const C_KEYWORDS = [
    'alignas','alignof','and','asm','auto','bool','break','case','catch','char',
    'class','const','constexpr','continue','decltype','default','delete','do',
    'double','else','enum','explicit','export','extern','false','float','for',
    'friend','goto','if','inline','int','long','mutable','namespace','new',
    'noexcept','nullptr','operator','or','private','protected','public',
    'register','return','short','signed','sizeof','static','struct','switch',
    'template','this','throw','true','try','typedef','typeid','typename',
    'union','unsigned','using','virtual','void','volatile','while',
  ];
  const C_TYPES = [
    'std','cin','cout','cerr','endl','printf','scanf','puts','gets','malloc',
    'free','memset','memcpy','strlen','strcpy','strcmp','sprintf','vector',
    'string','map','set','pair','queue','stack','deque','sort','swap','max',
    'min','abs','size_t','FILE','NULL','iostream','cstdio','algorithm',
  ];
  const PY_KEYWORDS = [
    'False','None','True','and','as','assert','async','await','break','class',
    'continue','def','del','elif','else','except','finally','for','from',
    'global','if','import','in','is','lambda','nonlocal','not','or','pass',
    'raise','return','try','while','with','yield',
  ];
  const PY_TYPES = [
    'print','input','int','float','str','bool','list','dict','set','tuple',
    'len','range','map','filter','sum','min','max','abs','sorted','enumerate',
    'zip','open','type','isinstance','round','divmod','self',
  ];

  const group = (words) => '\\b(?:' + words.join('|') + ')\\b';

  // String before comment: the two only compete at the same position for a
  // literal like "// x", where the string reading is the correct one.
  const RE_C = new RegExp([
    '(?<str>"(?:\\\\[\\s\\S]|[^"\\\\\\n])*"|\'(?:\\\\[\\s\\S]|[^\'\\\\\\n])*\')',
    '(?<cmt>//[^\\n]*|/\\*[\\s\\S]*?\\*/)',
    '(?<pre>^[ \\t]*#[A-Za-z_]+[^\\n]*)',
    '(?<num>\\b0[xX][0-9a-fA-F]+\\b|\\b\\d+(?:\\.\\d+)?(?:[eE][+-]?\\d+)?[uUlLfF]*\\b)',
    '(?<kw>' + group(C_KEYWORDS) + ')',
    '(?<typ>' + group(C_TYPES) + ')',
  ].join('|'), 'gm');

  const RE_PY = new RegExp([
    '(?<str>"""[\\s\\S]*?"""|\'\'\'[\\s\\S]*?\'\'\'' +
      '|"(?:\\\\[\\s\\S]|[^"\\\\\\n])*"|\'(?:\\\\[\\s\\S]|[^\'\\\\\\n])*\')',
    '(?<cmt>#[^\\n]*)',
    '(?<num>\\b0[xX][0-9a-fA-F]+\\b|\\b\\d+(?:\\.\\d+)?(?:[eE][+-]?\\d+)?\\b)',
    '(?<kw>' + group(PY_KEYWORDS) + ')',
    '(?<typ>' + group(PY_TYPES) + ')',
  ].join('|'), 'gm');

  const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;' };
  const esc = (s) => s.replace(/[&<>]/g, (c) => ESCAPES[c]);

  function highlight(code, lang) {
    const re = lang === 'python' ? RE_PY : RE_C;
    re.lastIndex = 0;
    let out = '';
    let last = 0;
    let m;
    while ((m = re.exec(code)) !== null) {
      if (m[0].length === 0) { re.lastIndex++; continue; } // never spin
      out += esc(code.slice(last, m.index));
      let cls = 'typ';
      for (const key in m.groups) {
        if (m.groups[key] !== undefined) { cls = key; break; }
      }
      out += '<span class="t-' + cls + '">' + esc(m[0]) + '</span>';
      last = m.index + m[0].length;
    }
    out += esc(code.slice(last));
    // The trailing newline gives the final (possibly empty) line height, so
    // the highlight layer stays aligned with the textarea.
    return out + '\n';
  }

  class CodeEditor {
    constructor(host, opts = {}) {
      this.lang = opts.language || 'cpp';
      this.onChange = opts.onChange || (() => {});
      this.onPaste = opts.onPaste || (() => {});

      this.root = document.createElement('div');
      this.root.className = 'cr-editor';

      this.gutter = document.createElement('div');
      this.gutter.className = 'cr-gutter';
      this.gutterInner = document.createElement('div');
      this.gutter.append(this.gutterInner);

      this.wrap = document.createElement('div');
      this.wrap.className = 'cr-wrap';
      this.hl = document.createElement('pre');
      this.hl.className = 'cr-hl';
      this.input = document.createElement('textarea');
      this.input.className = 'cr-input';
      this.input.spellcheck = false;
      this.input.autocapitalize = 'off';
      this.input.autocomplete = 'off';
      this.input.setAttribute('wrap', 'off');
      this.wrap.append(this.hl, this.input);

      this.root.append(this.gutter, this.wrap);
      host.replaceChildren(this.root);

      this.input.addEventListener('input', () => { this.repaint(); this.onChange(this.getValue()); });
      this.input.addEventListener('scroll', () => this.syncScroll());
      this.input.addEventListener('keydown', (e) => this.onKeyDown(e));
      this.input.addEventListener('paste', (e) => {
        const text = (e.clipboardData || global.clipboardData)?.getData('text') || '';
        if (text) {
          this.onPaste({ chars: text.length, lines: text.split('\n').length });
        }
        // Paste is intentionally NOT blocked: it is the designed input path.
      });
      this.repaint();
    }

    onKeyDown(e) {
      // Tab must indent, not move focus out of the editor.
      if (e.key === 'Tab') {
        e.preventDefault();
        this.insert('    ');
        return;
      }
      // Keep the previous line's indentation, as any IDE would.
      if (e.key === 'Enter') {
        const v = this.input.value;
        const pos = this.input.selectionStart;
        const lineStart = v.lastIndexOf('\n', pos - 1) + 1;
        const indent = (v.slice(lineStart, pos).match(/^[ \t]*/) || [''])[0];
        if (indent) {
          e.preventDefault();
          this.insert('\n' + indent);
        }
      }
    }

    insert(text) {
      const start = this.input.selectionStart;
      const end = this.input.selectionEnd;
      const v = this.input.value;
      this.input.value = v.slice(0, start) + text + v.slice(end);
      this.input.selectionStart = this.input.selectionEnd = start + text.length;
      this.repaint();
      this.onChange(this.getValue());
    }

    repaint() {
      const code = this.input.value;
      this.hl.innerHTML = highlight(code, this.lang);
      const lines = code.split('\n').length;
      if (this._lines !== lines) {
        this._lines = lines;
        let g = '';
        for (let i = 1; i <= lines; i++) g += i + '\n';
        this.gutterInner.textContent = g;
      }
      this.syncScroll();
    }

    syncScroll() {
      this.hl.scrollTop = this.input.scrollTop;
      this.hl.scrollLeft = this.input.scrollLeft;
      this.gutterInner.style.transform = 'translateY(' + -this.input.scrollTop + 'px)';
    }

    getValue() { return this.input.value; }

    setValue(code) {
      if (this.input.value === code) return;
      this.input.value = code || '';
      this.repaint();
    }

    setLanguage(lang) {
      this.lang = lang;
      this.repaint();
    }

    setReadOnly(ro) { this.input.readOnly = !!ro; }
    focus() { this.input.focus(); }
  }

  global.CodeEditor = CodeEditor;
  global.highlightCode = highlight; // exported for tests
})(window);
