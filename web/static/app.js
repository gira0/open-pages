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

  // Upload form: creates the site (an existing one is fine), then uploads the archive to it.
  function setupUpload() {
    var form = document.getElementById('upload-form');
    if (!form) { return; }
    var result = document.getElementById('upload-result');
    var progress = document.getElementById('upload-progress');
    form.addEventListener('submit', async function (evt) {
      evt.preventDefault();
      var name = form.elements.name.value;
      var file = form.elements.file.files[0];
      progress.value = 0;
      result.textContent = '';
      var created = await fetch('/v1/auth/sites', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({name: name})
      });
      if (!created.ok && created.status !== 409) {
        result.textContent = 'create site failed: ' + await created.text();
        return;
      }
      var data = new FormData();
      data.append('file', file);
      var xhr = new XMLHttpRequest();
      xhr.open('POST', '/v1/auth/sites/' + encodeURIComponent(name) + '/formupload');
      xhr.upload.onprogress = function (e) {
        if (e.lengthComputable) { progress.value = e.loaded / e.total * 100; }
      };
      xhr.onload = function () { result.textContent = xhr.status + ' ' + xhr.responseText; };
      xhr.onerror = function () { result.textContent = 'upload failed'; };
      xhr.send(data);
    });
  }

  setupTheme();
  setupNav();
  setupUpload();
})();
