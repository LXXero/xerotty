# xerotty TODO

## Done
- [x] unsafe paste dialog (phase 1 — yes/no works)
- [x] double-click word selection
- [x] tab rename
- [x] font_size_reset (Ctrl+Shift+0)
- [x] scroll_on_output freeze — viewport stays frozen when scrolled back, offset adjusts for new scrollback
- [x] double-click blank space — xfce4-terminal behavior: word=word, space=space, triple-click=line
- [x] fullscreen (F11) — fixed: uses SDL_WINDOW_FULLSCREEN_DESKTOP (0x1001) instead of real fullscreen
- [x] runtime theme switching — `set_theme:<name>` action now also updates SDL background color
- [x] Shift+Home / Shift+End — scroll_top / scroll_bottom actions
- [x] on_child_exit — close/hold/hold_on_error with proper child exit detection

## High Priority

- [ ] **unlimited scrollback** — disk swap (spec 6.5); wants it configurable in the config dialog
- [ ] **image paste / Kitty protocol** — test what works/doesn't; also Shift+Enter support (needed for claude cli etc)
- [ ] **context menu config + submenus** — no menu config at all right now; needs config dialog work first
- [ ] **new tab inherits CWD** — `$XEROTTY_CWD` / open_tab should use parent tab's working dir; config option

## Low Priority / Later

- [ ] select_all / clear_scrollback / reset_terminal actions — add once context menu is configurable
- [ ] config dialog (prerequisite for several above)
- [ ] Tmux helpers (spec 6.13) — explicitly last
