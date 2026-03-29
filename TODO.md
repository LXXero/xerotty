# xerotty TODO

## Done
- [x] unsafe paste dialog (phase 1 — yes/no works)
- [x] double-click word selection
- [x] tab rename
- [x] font_size_reset (Ctrl+Shift+0)

## High Priority

- [ ] **scroll_on_output freeze** — when scrolled up, new output should NOT drag viewport down; stay in place with a "new output below" indicator
- [ ] **double-click blank space → select line** — clicking blank space or non-word chars should select the whole line
- [ ] **fullscreen (F11)** — option exists in config but doesn't actually work
- [ ] **runtime theme switching** — `set_theme:<name>` action; user wants real terminal themes badly (current theme: 🤢)
- [ ] **unlimited scrollback** — disk swap (spec 6.5); wants it configurable in the config dialog
- [ ] **image paste / Kitty protocol** — test what works/doesn't; also Shift+Enter support (needed for claude cli etc)

## Medium Priority

- [ ] **on_child_exit** — when shell exits: close tab / hold / hold_on_error; configurable in config dialog
- [ ] **Shift+Home / Shift+End** — jump to top/bottom of scrollback
- [ ] **context menu config + submenus** — no menu config at all right now; needs config dialog work first
- [ ] **new tab inherits CWD** — `$XEROTTY_CWD` / open_tab should use parent tab's working dir; config option

## Low Priority / Later

- [ ] select_all / clear_scrollback / reset_terminal actions — add once context menu is configurable
- [ ] config dialog (prerequisite for several above)
- [ ] Tmux helpers (spec 6.13) — explicitly last
