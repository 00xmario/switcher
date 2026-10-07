// The Switcher mark: an S-shaped track that is also a switch, its knob in the
// "on" position. playMark() animates the knob drawing the S, from the bottom
// end to the on position, for moments that switch something.
const TRACK = 'M68 28H44A11 11 0 0 0 44 50H56A11 11 0 0 1 56 72H32';
// The same track from the bottom end, relative to the knob's resting place.
const RUN = 'M-36 44H-12A11 11 0 0 0 -12 22H-24A11 11 0 0 1 -24 0H0';
let serial = 0;

// switcherMark returns the mark as inline SVG. tile adds the dark app-icon
// square behind it; mono draws it in currentColor.
// id names the gradients and mask; a view that re-renders and compares
// markup passes a stable one.
export function switcherMark({ size = 20, tile = false, mono = false, label = '', id = `sw-mark-${++serial}` } = {}) {
  const track = mono ? 'currentColor' : `url(#${id}-g)`;
  // On the dark tile the knob is pearl white; elsewhere it follows the theme
  // (--mark-knob), so it stays visible on light backgrounds.
  const knob = mono ? 'currentColor' : tile ? '#f4fff8' : 'var(--mark-knob, #f4fff8)';
  const timing = 'dur=".72s" begin="indefinite" fill="freeze" calcMode="spline" keyTimes="0;1" keySplines=".45 0 .2 1"';
  return `<svg class="switcher-mark${tile ? ' is-tile' : ''}" viewBox="${tile ? '0 0 100 100' : '22 12 62 76'}" width="${size}" height="${size}" ${label ? `role="img" aria-label="${label}"` : 'aria-hidden="true"'} focusable="false">
    <defs>
      ${mono ? '' : `<linearGradient id="${id}-g" gradientUnits="userSpaceOnUse" x1="30" y1="22" x2="62" y2="80"><stop offset="0" stop-color="#d4f76e"/><stop offset=".5" stop-color="#43dc8d"/><stop offset="1" stop-color="#10a89a"/></linearGradient>`}
      ${tile ? `<linearGradient id="${id}-bg" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#22332b"/><stop offset="1" stop-color="#0a120e"/></linearGradient>` : ''}
      <mask id="${id}-cut" maskUnits="userSpaceOnUse" x="0" y="0" width="100" height="100"><rect width="100" height="100" fill="#fff"/><circle cx="68" cy="28" r="13.4" fill="#000"/></mask>
    </defs>
    ${tile ? `<rect width="100" height="100" rx="23" fill="url(#${id}-bg)"/><rect x="1" y="1" width="98" height="98" rx="22" fill="none" stroke="#fff" stroke-opacity=".1" stroke-width="2"/>` : ''}
    <g ${tile ? 'transform="translate(50 50) scale(1.05) translate(-52.9 -47.1)"' : ''}>
      <path d="${TRACK}" fill="none" stroke="${track}" stroke-width="10.5" stroke-linecap="round" stroke-linejoin="round" mask="url(#${id}-cut)" pathLength="1" stroke-dasharray="1 1">
        <animate attributeName="stroke-dashoffset" from="-1" to="0" ${timing}/>
      </path>
      <g transform="translate(68 28)"><circle r="11" style="fill: ${knob}">
        <animateMotion path="${RUN}" ${timing}/>
      </circle></g>
    </g>
  </svg>`;
}

// playMark runs the switch animation once on every mark inside root.
export function playMark(root) {
  if (!root || globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return;
  for (const animation of root.querySelectorAll('.switcher-mark animate, .switcher-mark animateMotion')) {
    try { animation.beginElement(); } catch { /* SMIL unavailable: the mark stays still */ }
  }
}
