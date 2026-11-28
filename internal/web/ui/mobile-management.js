// Management rows expose actions without committing them on a gesture.
// Store transient pointer data on the actual opener, not on the Alpine app.
const gestures = new WeakMap();

export function managementRowGesture(event, reveal, hold = false) {
  const button = event.currentTarget;
  const gesture = gestures.get(button);
  if (event.type === 'pointerdown') {
    clearTimeout(gesture?.timer);
    gestures.delete(button);
    if (event.pointerType === 'mouse' || !event.isPrimary || event.button !== 0) return;
    const next = { pointer: event.pointerId, x: event.clientX, y: event.clientY };
    gestures.set(button, next);
    if (hold) next.timer = setTimeout(() => {
      if (button.isConnected && button.getClientRects().length && reveal()) next.consumed = true;
    }, 500);
  } else if (gesture?.pointer === event.pointerId) {
    const dx = event.clientX - gesture.x, dy = event.clientY - gesture.y;
    if (event.type === 'pointermove') {
      if (Math.hypot(dx, dy) >= 10) clearTimeout(gesture.timer);
      if (Math.abs(dy) >= 12 && Math.abs(dy) >= Math.abs(dx)) gesture.scrolling = true;
      return;
    }
    clearTimeout(gesture.timer);
    if (event.type === 'pointerup' && !gesture.scrolling && !gesture.consumed && dx <= -40 && -dx > Math.abs(dy) * 1.5) {
      gesture.consumed = !!reveal();
    }
    if (!gesture.consumed) gestures.delete(button);
  }
}

export function managementRowClickConsumed(event) {
  // Keyboard and assistive-technology clicks are not follow-up pointer clicks.
  if (!event.detail || !gestures.get(event.currentTarget)?.consumed) return false;
  gestures.delete(event.currentTarget);
  return true;
}
