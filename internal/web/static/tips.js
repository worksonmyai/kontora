// Hover cards for [data-tip], [data-tip-t] and [data-tip-e]. Extracted from
// index.html so the document needs no inline script under the CSP.
(function() {
  var dismissDetachedTips = [];
  var generatedLabels = new WeakMap();
  var isolatedInfoClicks = new WeakSet();

  // fillFn replaces the default single-line text for a tip that has
  // structure. Clearing the inline transform on hide lets the CSS resting
  // transform apply again, so the next hover animates in.
  function setupTip(id, selector, attr, positionFn, fillFn) {
    var el = document.getElementById(id);
    var timer, open = null, rearm = false, pinned = false;
    var described = null;
    el.setAttribute('role', 'tooltip');
    el.setAttribute('aria-hidden', 'true');

    function show(trig) {
      var text = trig.getAttribute(attr);
      if (!text) return;
      clearTimeout(timer);
      hide();
      open = trig;
      described = trig.getAttribute('aria-describedby');
      var ids = (described || '').split(/\s+/).filter(Boolean);
      if (ids.indexOf(id) < 0) ids.push(id);
      trig.setAttribute('aria-describedby', ids.join(' '));
      el.setAttribute('aria-hidden', 'false');
      if (fillFn) fillFn(el, trig); else el.textContent = text;
      el.style.opacity = '1';
      el.style.transform = 'none';
      el.style.left = '0'; el.style.top = '0';
      positionFn(el, trig.getBoundingClientRect());
    }

    function hide() {
      clearTimeout(timer);
      if (open) {
        if (described) open.setAttribute('aria-describedby', described);
        else open.removeAttribute('aria-describedby');
      }
      open = null;
      described = null;
      pinned = false;
      el.setAttribute('aria-hidden', 'true');
      el.style.opacity = '0';
      el.style.transform = '';
    }

    dismissDetachedTips.push(function() {
      if (open && !open.isConnected) hide();
    });

    document.addEventListener('mouseenter', function(e) {
      var trig = e.target.closest && e.target.closest(selector);
      if (trig && !pinned) show(trig);
    }, true);

    document.addEventListener('focusin', function(e) {
      var trig = e.target.closest && e.target.closest(selector);
      if (trig) show(trig);
    });

    document.addEventListener('focusout', function(e) {
      if (open && e.target === open && !pinned) hide();
    });

    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape' || !open) return;
      hide();
      rearm = false;
      e.preventDefault();
      e.stopImmediatePropagation();
    }, true);

    document.addEventListener('scroll', hide, true);
    window.addEventListener('resize', hide);

    document.addEventListener('mouseleave', function(e) {
      if (!e.target.closest || !e.target.closest(selector) || pinned || open === document.activeElement) return;
      timer = setTimeout(hide, 80);
    }, true);

    // A chip that opens a ticket is off the page by the time the pointer
    // leaves it, and a node taken out of the DOM fires no mouseleave, so the
    // card would hang there until the page is reloaded. Drop it on the click
    // and let the next pointer move decide whether it comes back, which is
    // what shows the card of whatever the click drew under the cursor.
    document.addEventListener('click', function(e) {
      var trig = e.target?.closest && e.target.closest(selector);
      if (trig?.matches('button.tip, [data-tip-info]')) {
        if (pinned && open === trig) hide();
        else { show(trig); pinned = true; }
        rearm = false;
        return;
      }
      if (!open) return;
      clearTimeout(timer);
      hide();
      rearm = true;
    }, true);

    document.addEventListener('mousemove', function(e) {
      if (!rearm || pinned) return;
      rearm = false;
      var trig = e.target.closest && e.target.closest(selector);
      if (trig) show(trig);
    }, true);
  }

  function isolateInfoClick(node) {
    if (isolatedInfoClicks.has(node)) return;
    isolatedInfoClicks.add(node);
    // Keep the trigger's own click handlers, but do not activate a containing ticket card.
    node.addEventListener('click', function(e) { e.stopPropagation(); });
  }

  function prepareTips(root) {
    var selector = '[data-tip], [data-tip-t], [data-tip-e]';
    var nodes = Array.from(root.querySelectorAll(selector));
    if (root.matches?.(selector)) nodes.unshift(root);
    nodes.forEach(function(node) {
      var text = node.getAttribute('data-tip') || node.getAttribute('data-tip-t') || node.getAttribute('data-tip-e');
      if (!text) return;
      if (node.matches('button.tip')) isolateInfoClick(node);
      if (!node.hasAttribute('data-tip-info') && node.matches('button, a[href], input, select, textarea, [role="button"], [role="link"]')) return;
      if (!node.hasAttribute('tabindex')) node.setAttribute('tabindex', '0');
      node.setAttribute('role', 'button');
      node.setAttribute('data-tip-info', '');
      isolateInfoClick(node);
      if (!node.hasAttribute('aria-label') || generatedLabels.get(node) === node.getAttribute('aria-label')) {
        node.setAttribute('aria-label', text);
        generatedLabels.set(node, text);
      }
    });
  }
  if (document.querySelectorAll) prepareTips(document);
  if (typeof MutationObserver === 'function') {
    new MutationObserver(function(records) {
      records.forEach(function(record) {
        if (record.type === 'attributes') prepareTips(record.target);
        else record.addedNodes.forEach(function(node) { if (node.nodeType === 1) prepareTips(node); });
      });
      dismissDetachedTips.forEach(function(dismiss) { dismiss(); });
    }).observe(document.body, { childList: true, subtree: true, attributes: true,
      attributeFilter: ['data-tip', 'data-tip-t', 'data-tip-e'] });
  }
  // Information controls can sit inside ticket cards. Consume activation before the card's delegated handlers run.
  document.addEventListener('keydown', function(e) {
    if ((e.key === 'Enter' || e.key === ' ') && e.target.matches?.('[data-tip-info]:not(button)')) {
      e.preventDefault();
      e.stopPropagation();
      e.target.click();
    }
  }, true);

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
