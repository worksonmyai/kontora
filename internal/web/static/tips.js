// Hover cards for [data-tip], [data-tip-t] and [data-tip-e]. Extracted from
// index.html so the document needs no inline script under the CSP.
(function() {
  // fillFn replaces the default single-line text for a tip that has
  // structure. Clearing the inline transform on hide lets the CSS resting
  // transform apply again, so the next hover animates in.
  function setupTip(id, selector, attr, positionFn, fillFn) {
    var el = document.getElementById(id);
    var timer, open = null, rearm = false;

    function show(trig) {
      var text = trig.getAttribute(attr);
      if (!text) return;
      clearTimeout(timer);
      open = trig;
      if (fillFn) fillFn(el, trig); else el.textContent = text;
      el.style.opacity = '1';
      el.style.transform = 'none';
      el.style.left = '0'; el.style.top = '0';
      positionFn(el, trig.getBoundingClientRect());
    }

    function hide() {
      open = null;
      el.style.opacity = '0';
      el.style.transform = '';
    }

    document.addEventListener('mouseenter', function(e) {
      var trig = e.target.closest && e.target.closest(selector);
      if (trig) show(trig);
    }, true);

    document.addEventListener('mouseleave', function(e) {
      if (!e.target.closest || !e.target.closest(selector)) return;
      timer = setTimeout(hide, 80);
    }, true);

    // A chip that opens a ticket is off the page by the time the pointer
    // leaves it, and a node taken out of the DOM fires no mouseleave, so the
    // card would hang there until the page is reloaded. Drop it on the click
    // and let the next pointer move decide whether it comes back, which is
    // what shows the card of whatever the click drew under the cursor.
    document.addEventListener('click', function() {
      if (!open) return;
      clearTimeout(timer);
      hide();
      rearm = true;
    }, true);

    document.addEventListener('mousemove', function(e) {
      if (!rearm) return;
      rearm = false;
      var trig = e.target.closest && e.target.closest(selector);
      if (trig) show(trig);
    }, true);
  }

  // Any [data-tip], not just the round .tip button: this is the wrapping,
  // width-capped card, which is what a full sentence needs.
  setupTip('global-tip', '[data-tip]', 'data-tip', function(el, r) {
    var tw = el.offsetWidth, th = el.offsetHeight;
    var left = r.left + r.width / 2 - tw / 2;
    var top = r.top - th - 6;
    if (left < 4) left = 4;
    if (left + tw > window.innerWidth - 4) left = window.innerWidth - 4 - tw;
    if (top < 4) top = r.bottom + 6;
    el.style.left = left + 'px';
    el.style.top = top + 'px';
  });

  setupTip('global-tip-t', '[data-tip-t]', 'data-tip-t', function(el, r) {
    var tw = el.offsetWidth, th = el.offsetHeight;
    var left = r.right - tw;
    var top = r.top - th - 4;
    if (left < 4) left = 4;
    if (top < 4) top = r.bottom + 4;
    el.style.left = left + 'px';
    el.style.top = top + 'px';
  });

  // Entity chips in summary prose. Left-aligned to the chip rather than
  // centred, because the card is wider than the word it describes.
  setupTip('global-tip-e', '[data-tip-e]', 'data-tip-e', function(el, r) {
    var tw = el.offsetWidth, th = el.offsetHeight;
    var left = r.left;
    var top = r.top - th - 10;
    if (left < 12) left = 12;
    if (left + tw > window.innerWidth - 12) left = window.innerWidth - 12 - tw;
    if (top < 8) top = r.bottom + 10;
    el.style.left = left + 'px';
    el.style.top = top + 'px';
  }, function(el, trig) {
    var hint = trig.getAttribute('data-tip-e-hint') || '';
    var tag = trig.getAttribute('data-tip-e-tag') || '';
    var title = el.children[0];
    title.children[0].textContent = tag;
    title.children[0].style.display = tag ? '' : 'none';
    title.children[1].textContent = trig.getAttribute('data-tip-e');
    // One card serves every chip, and no chip is an ancestor of it, so the
    // hue is mirrored here on every hover. A write left out on one branch
    // leaks the last chip's tag or colour into the next card.
    el.setAttribute('data-pipe-color', trig.getAttribute('data-tip-e-tag-color') || 'none');
    el.children[1].textContent = trig.getAttribute('data-tip-e-body') || '';
    el.children[2].textContent = hint;
    el.children[2].style.display = hint ? '' : 'none';
  });
})();
