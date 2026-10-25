// Behaviour for the UI pages. Kept out of the templates so the pages can run under a
// Content-Security-Policy without 'unsafe-inline'.
(function () {
  'use strict';

  var themeKey = 'open-pages-theme';
  var root = document.documentElement;

  // Theme toggle: flips dark mode and remembers the choice. Without a saved choice the
  // page follows the system setting, also when it changes while the page is open.
  function setupTheme() {
    var button = document.getElementById('theme-toggle');
    if (!button) { return; }
    var icon = button.querySelector('i');
    function show() {
      var dark = root.classList.contains('pf-v6-theme-dark');
      button.setAttribute('aria-pressed', dark ? 'true' : 'false');
      icon.className = dark ? 'fas fa-sun' : 'fas fa-moon';
    }
    button.addEventListener('click', function () {
      var dark = !root.classList.contains('pf-v6-theme-dark');
      root.classList.toggle('pf-v6-theme-dark', dark);
      try { localStorage.setItem(themeKey, dark ? 'dark' : 'light'); } catch (e) { /* storage blocked */ }
      show();
    });
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function (e) {
      var saved = null;
      try { saved = localStorage.getItem(themeKey); } catch (err) { /* storage blocked */ }
      if (!saved) { root.classList.toggle('pf-v6-theme-dark', e.matches); show(); }
    });
    show();
  }

  // Sidebar toggle in the masthead. Without a modifier class PatternFly shows the sidebar
  // on wide screens (75rem, its xl breakpoint) and hides it on narrow ones; the toggle
  // pins it open or closed.
  function setupNav() {
    var button = document.getElementById('nav-toggle');
    var sidebar = document.getElementById('sidebar');
    if (!button || !sidebar) { return; }
    var wide = window.matchMedia('(min-width: 75rem)');
    function isOpen() {
      if (sidebar.classList.contains('pf-m-expanded')) { return true; }
      if (sidebar.classList.contains('pf-m-collapsed')) { return false; }
      return wide.matches;
    }
    function show() { button.setAttribute('aria-expanded', isOpen() ? 'true' : 'false'); }
    button.addEventListener('click', function () {
      var open = !isOpen();
      sidebar.classList.toggle('pf-m-expanded', open);
      sidebar.classList.toggle('pf-m-collapsed', !open);
      show();
    });
    wide.addEventListener('change', function () {
      // A choice made for one screen size doesn't carry over to the other.
      sidebar.classList.remove('pf-m-expanded', 'pf-m-collapsed');
      show();
    });
    show();
  }

  // Shows a PatternFly inline alert in container, replacing the previous one. kind is
  // 'danger' or 'success'. The text is set with textContent, never as HTML.
  var alertIcons = {danger: 'fa-exclamation-circle', success: 'fa-check-circle'};
  var alertLabels = {danger: 'Danger alert:', success: 'Success alert:'};
  function showAlert(container, kind, title, detail) {
    var alert = document.createElement('div');
    alert.className = 'pf-v6-c-alert pf-m-inline pf-m-' + kind;
    alert.setAttribute('role', kind === 'danger' ? 'alert' : 'status');
    var icon = document.createElement('div');
    icon.className = 'pf-v6-c-alert__icon';
    var i = document.createElement('i');
    i.className = 'fas fa-fw ' + alertIcons[kind];
    i.setAttribute('aria-hidden', 'true');
    icon.appendChild(i);
    var heading = document.createElement('p');
    heading.className = 'pf-v6-c-alert__title';
    var sr = document.createElement('span');
    sr.className = 'pf-v6-screen-reader';
    sr.textContent = alertLabels[kind];
    heading.appendChild(sr);
    heading.appendChild(document.createTextNode(title));
    alert.appendChild(icon);
    alert.appendChild(heading);
    if (detail) {
      var desc = document.createElement('div');
      desc.className = 'pf-v6-c-alert__description';
      desc.textContent = detail;
      alert.appendChild(desc);
    }
    container.replaceChildren(alert);
  }

  // Upload form: creates the site (an existing one is fine), then uploads the archive to it.
  function setupUpload() {
    var form = document.getElementById('upload-form');
    if (!form) { return; }
    var result = document.getElementById('upload-result');
    var progress = document.getElementById('upload-progress');
    var bar = progress.querySelector('[role=progressbar]');
    var indicator = document.getElementById('upload-progress-bar');
    var measure = document.getElementById('upload-progress-measure');
    var fileInput = document.getElementById('file');
    var fileName = document.getElementById('file-name');
    var clearButton = document.getElementById('file-clear');

    function setProgress(pct) {
      var p = Math.max(0, Math.min(100, Math.round(pct)));
      indicator.style.width = p + '%';
      bar.setAttribute('aria-valuenow', String(p));
      bar.setAttribute('aria-valuetext', p + '%');
      measure.textContent = p + '%';
    }
    function showFile() {
      var f = fileInput.files[0];
      fileName.value = f ? f.name : '';
      clearButton.disabled = !f;
    }
    document.getElementById('file-browse').addEventListener('click', function () { fileInput.click(); });
    fileName.addEventListener('click', function () { fileInput.click(); });
    clearButton.addEventListener('click', function () { fileInput.value = ''; showFile(); });
    fileInput.addEventListener('change', showFile);
    form.addEventListener('reset', function () { setTimeout(showFile, 0); });

    form.addEventListener('submit', async function (evt) {
      evt.preventDefault();
      var name = form.elements.name.value;
      var file = fileInput.files[0];
      result.replaceChildren();
      if (!file) {
        showAlert(result, 'danger', 'Choose an archive to upload.');
        return;
      }
      setProgress(0);
      progress.hidden = false;
      var created;
      try {
        created = await fetch('/v1/auth/sites', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({name: name})
        });
      } catch (e) {
        progress.hidden = true;
        showAlert(result, 'danger', 'Creating the site failed.', 'Network error');
        return;
      }
      if (!created.ok && created.status !== 409) {
        progress.hidden = true;
        showAlert(result, 'danger', 'Creating the site failed.', await created.text());
        return;
      }
      var data = new FormData();
      data.append('file', file);
      var xhr = new XMLHttpRequest();
      xhr.open('POST', '/v1/auth/sites/' + encodeURIComponent(name) + '/formupload');
      xhr.upload.onprogress = function (e) {
        if (e.lengthComputable) { setProgress(e.loaded / e.total * 100); }
      };
      xhr.onload = function () {
        if (xhr.status >= 200 && xhr.status < 300) {
          setProgress(100);
          showAlert(result, 'success', 'Upload complete.', xhr.responseText);
        } else {
          progress.hidden = true;
          showAlert(result, 'danger', 'Upload failed (' + xhr.status + ').', xhr.responseText);
        }
      };
      xhr.onerror = function () {
        progress.hidden = true;
        showAlert(result, 'danger', 'Upload failed.', 'Network error');
      };
      xhr.send(data);
    });
  }

  // Sign-in for non-public sites: a site sends the browser to /index?next=/v1/auth/site-login?...
  // and expects to be sent back there after signing in. Only that one relative path is ever
  // followed. Single sign-on leaves the page, so next is kept in sessionStorage meanwhile.
  var nextKey = 'open-pages-next';
  var nextPrefix = '/v1/auth/site-login?';
  function safeNext(v) {
    return typeof v === 'string' && v.indexOf(nextPrefix) === 0 && !/[\\\u0000-\u001f\u007f]/.test(v) ? v : null;
  }
  function takeNext() {
    var next = safeNext(new URLSearchParams(location.search).get('next'));
    try {
      if (next) { sessionStorage.setItem(nextKey, JSON.stringify({next: next, at: Date.now()})); }
      var saved = JSON.parse(sessionStorage.getItem(nextKey) || 'null');
      next = saved && Date.now() - saved.at < 600000 ? safeNext(saved.next) : null;
    } catch (e) { /* storage blocked */ }
    return next;
  }
  function goNext(next) {
    try { sessionStorage.removeItem(nextKey); } catch (e) { /* storage blocked */ }
    if (next) { location.assign(next); } else { location.reload(); }
  }
  function setupLogin() {
    if (!document.getElementById('sign-in')) { return; } // only the index page
    var form = document.getElementById('login-form');
    var next = takeNext();
    if (next && !new URLSearchParams(location.search).has('next')) {
      // Back from single sign-on: continue if the sign-in worked.
      fetch('/v1/auth/user').then(function (r) { if (r.ok) { goNext(next); } });
    }
    if (!form) { return; }
    var result = document.getElementById('login-result');
    form.addEventListener('submit', async function (evt) {
      evt.preventDefault();
      var resp;
      try {
        resp = await fetch(form.action, {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({email: form.elements.email.value, password: form.elements.password.value})
        });
      } catch (e) {
        showAlert(result, 'danger', 'Sign-in failed.', 'Network error');
        return;
      }
      if (resp.ok) { goNext(next); } else { showAlert(result, 'danger', 'Sign-in failed.', await resp.text()); }
    });
  }

  setupTheme();
  setupNav();
  setupLogin();
  setupUpload();
})();
