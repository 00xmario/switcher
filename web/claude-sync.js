const title = 'Sync Claude Code sessions between accounts';
const explanation = 'Share new Claude Code chats across accounts. Confirmation required: this closes and reopens Claude Desktop.';
const icons = {
  idle: '<path d="M4 8h16m-4-4 4 4-4 4M20 16H4m4-4-4 4 4 4"/>',
  success: '<path d="m5 12 4 4L19 6"/>',
  error: '<path d="M12 7v6m0 4h.01"/><circle cx="12" cy="12" r="9"/>',
};
const escape = value => String(value).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// State survives dashboard polling and tab changes. Both render() and an
// in-flight completion paint from this same controller.
export function createClaudeSync({ request, confirm, paint, schedule = setTimeout }) {
  let phase = 'idle', detail = '', timerVersion = 0, confirming = false;
  function html() {
    const icon = icons[phase] || icons.idle;
    return `<span class="claude-sync-control">
      <button type="button" class="claude-sync-button ${phase}" data-claude-sync
        aria-label="${title}" aria-busy="${phase === 'running'}" ${phase === 'running' ? 'disabled' : ''}
        title="${title}\n${explanation}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${icon}</svg></button>
      ${phase === 'success' ? `<span class="claude-sync-feedback" role="status">Claude sessions synced<span>${escape(detail)}</span></span>` : ''}
      ${phase === 'error' ? `<details class="claude-sync-feedback error"><summary role="alert">Sync failed · Details</summary><pre>${escape(detail)}</pre></details>` : ''}
      ${phase === 'running' ? '<span class="sr-only" role="status">Syncing Claude sessions. Claude Desktop will reopen when finished.</span>' : ''}
    </span>`;
  }
  async function run() {
    if (phase === 'running' || confirming) return;
    confirming = true;
    try {
      if (!await confirm()) return;
    } finally {
      confirming = false;
    }
    const version = ++timerVersion;
    phase = 'running'; detail = ''; paint();
    try {
      const response = await request();
      const body = await response.json();
      if (!response.ok) throw new Error([body.error, body.details, body.result?.backup && `Backup: ${body.result.backup}`].filter(Boolean).join('\n'));
      phase = 'success';
      detail = body.detail || 'No new sessions to share';
      schedule(() => { if (timerVersion === version) { phase = 'idle'; detail = ''; paint(); } }, 6000);
    } catch (error) {
      phase = 'error'; detail = error.message || 'Could not reach Switcher';
    }
    paint();
  }
  return { html, run };
}
