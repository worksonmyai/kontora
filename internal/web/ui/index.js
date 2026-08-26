// Bundle entry. esbuild compiles this and everything it imports into one IIFE
// served as /app.js.
//
// Each import below is a mixin factory returning part of one flat Alpine
// component, the shape settings.js and stats.js already had. They are merged
// into one object, so `this` inside any of them is the whole component and a
// method may call across module lines freely. No two mixins may define the
// same key; merge() throws if they do.
import { kontoraActivity } from './activity.js';
import { kontoraApp } from './app.js';
import { kontoraArchive, archiveDerive } from './archive.js';
import { kontoraAssistant, assistantStream } from './assistant.js';
import { kontoraBoard } from './board.js';
import { kontoraCreate } from './create.js';
import { kontoraDetail } from './detail.js';
import { kontoraFilter } from './filter.js';
import { kontoraMarkdown } from './markdown.js';
import { kontoraMobile } from './mobile.js';
import { kontoraNotes } from './notes.js';
import { kontoraPalette } from './palette.js';
import { kontoraRelations } from './relations.js';
import { kontoraSettings } from './settings.js';
import { statsDerive, kontoraStats } from './stats.js';
import { kontoraTerminal, termState } from './terminal.js';
import { kontoraTickets } from './tickets.js';

// Object.assign would read each source's getters as it copied them, and
// `columns` and `createPreviewYaml` would then run against a half-built
// component. Copying descriptors keeps a getter a getter, so it evaluates on
// the merged object the way it did when all of this was one literal.
//
// Throwing on a repeated key is the point of doing this by hand: a silent
// overwrite loses whichever method was defined first, and the symptom is a
// dead button rather than an error. Failing here fails wherever the bundle
// runs, browser included, not only under the node test.
function merge(...mixins) {
  const component = {};
  for (const mixin of mixins) {
    for (const key of Reflect.ownKeys(mixin)) {
      if (Object.hasOwn(component, key)) throw new Error(`kontora(): two mixins define ${String(key)}`);
    }
    Object.defineProperties(component, Object.getOwnPropertyDescriptors(mixin));
  }
  return component;
}

function kontora() {
  return merge(
    kontoraApp(),
    kontoraTickets(),
    kontoraCreate(),
    kontoraDetail(),
    kontoraBoard(),
    kontoraFilter(),
    kontoraTerminal(),
    kontoraPalette(),
    kontoraMarkdown(),
    kontoraNotes(),
    kontoraRelations(),
    kontoraActivity(),
    kontoraMobile(),
    kontoraSettings(),
    kontoraStats(),
    kontoraArchive(),
    kontoraAssistant(),
  );
}

// An IIFE encapsulates everything, so the values reached from outside it have
// to be assigned explicitly:
//
//   kontora         index.html's x-data="kontora()"
//   termState       the xterm handles, which the node suite drives directly
//   assistantStream the assistant's EventSource handle, driven the same way
//   statsDerive     the stats bucketing, which the node suite drives directly
//   archiveDerive   the archive's filter and sort, driven the same way
//
// Going the other way, theme.js stays a separate pre-paint script, so
// getStoredTheme, setStoredTheme and applyTheme are free variables here and
// resolve off the global at call time.
globalThis.kontora = kontora;
globalThis.termState = termState;
globalThis.assistantStream = assistantStream;
globalThis.statsDerive = statsDerive;
globalThis.archiveDerive = archiveDerive;
