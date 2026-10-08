import {AbsoluteFill, useCurrentFrame, useVideoConfig} from 'remotion';
import {steps, Line} from './script';

const FPS = 20;
const TYPE_CPS = 22; // characters per second while typing a command
const LINE_INTERVAL = 0.09; // seconds between output lines
const PAUSE_AFTER = 1.4; // seconds to read a step's output
const INTRO = 0.6;
const OUTRO = 2.2;
const ROWS = 19;

type Event = {at: number; kind: 'prompt' | 'type' | 'line' | 'clear'; text?: string; tone?: Line['tone']; chars?: number};

// Lay the steps out on a timeline once, in seconds.
function timeline(): {events: Event[]; total: number} {
  const events: Event[] = [];
  let t = INTRO;
  steps.forEach((step, i) => {
    if (i > 0) {
      events.push({at: t, kind: 'clear'});
      t += 0.3;
    }
    events.push({at: t, kind: 'prompt'});
    t += 0.4;
    events.push({at: t, kind: 'type', text: step.command, chars: step.command.length});
    t += step.command.length / TYPE_CPS + 0.35;
    t += step.work;
    for (const line of step.lines) {
      events.push({at: t, kind: 'line', text: line.text, tone: line.tone});
      t += LINE_INTERVAL;
    }
    t += PAUSE_AFTER;
  });
  return {events, total: t + OUTRO};
}

const {events, total} = timeline();
export const totalFrames = Math.ceil(total * FPS);

const colors = {
  bg: '#0f151b',
  fg: '#e2e8ee',
  dim: '#94a2b0',
  ok: '#5fcf86',
  warn: '#f0b94b',
  info: '#8fb8ff',
  prompt: '#f08a4b',
  cmd: '#ffffff',
};

function toneColor(tone: Line['tone']): string {
  switch (tone) {
    case 'ok':
      return colors.ok;
    case 'warn':
      return colors.warn;
    case 'info':
      return colors.info;
    case 'dim':
      return colors.dim;
    case 'cmd':
      return colors.cmd;
    default:
      return colors.fg;
  }
}

export const Demo = () => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const now = frame / fps;

  // Replay the events up to now into screen rows.
  type Row = {text: string; tone?: Line['tone']; prompt?: boolean};
  let rows: Row[] = [];
  let typing: {text: string; shown: number} | null = null;
  for (const e of events) {
    if (e.at > now) break;
    switch (e.kind) {
      case 'clear':
        rows = [];
        typing = null;
        break;
      case 'prompt':
        rows.push({text: '', prompt: true});
        typing = null;
        break;
      case 'type': {
        const shown = Math.min(e.chars!, Math.floor((now - e.at) * TYPE_CPS));
        rows[rows.length - 1] = {text: e.text!.slice(0, shown), prompt: true, tone: 'cmd'};
        typing = shown < e.chars! ? {text: e.text!, shown} : null;
        break;
      }
      case 'line':
        rows.push({text: e.text!, tone: e.tone});
        break;
    }
  }
  const visible = rows.slice(Math.max(0, rows.length - ROWS));
  const cursorOn = Math.floor(now * 2.5) % 2 === 0;
  const lastIsPrompt = visible.length > 0 && visible[visible.length - 1].prompt;

  return (
    <AbsoluteFill style={{backgroundColor: colors.bg, fontFamily: 'ui-monospace, Menlo, "SF Mono", Consolas, monospace', padding: 0}}>
      <div style={{display: 'flex', alignItems: 'center', gap: 8, padding: '14px 18px', borderBottom: '1px solid #243040'}}>
        <span style={{width: 12, height: 12, borderRadius: 6, background: '#ff5f57', display: 'inline-block'}} />
        <span style={{width: 12, height: 12, borderRadius: 6, background: '#febc2e', display: 'inline-block'}} />
        <span style={{width: 12, height: 12, borderRadius: 6, background: '#28c840', display: 'inline-block'}} />
        <span style={{color: colors.dim, fontSize: 14, marginLeft: 12}}>shop — anyship</span>
      </div>
      <div style={{padding: '16px 22px', fontSize: 17, lineHeight: '26px', color: colors.fg, whiteSpace: 'pre'}}>
        {visible.map((r, i) => (
          <div key={i} style={{color: toneColor(r.tone), fontWeight: r.tone === 'bold' ? 700 : 400}}>
            {r.prompt ? <span style={{color: colors.prompt}}>$ </span> : null}
            {r.text === '' && !r.prompt ? '\u00a0' : r.text}
            {r.prompt && i === visible.length - 1 && (typing || lastIsPrompt) && cursorOn ? (
              <span style={{background: colors.fg, color: colors.bg}}>&nbsp;</span>
            ) : null}
          </div>
        ))}
      </div>
    </AbsoluteFill>
  );
};
