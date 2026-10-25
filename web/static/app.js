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

  // ---- Site management (/manage) ----

  var visibilityLabels = {
    public: 'Public (anyone)',
    authenticated: 'Signed-in users',
    restricted: 'Owner and group members'
  };

  // el builds an element. attrs sets attributes, text sets textContent (never HTML).
  function el(tag, attrs, text) {
    var node = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) { node.setAttribute(k, attrs[k]); });
    if (text !== undefined) { node.textContent = text; }
    return node;
  }

  function buttonEl(label, classes, onClick) {
    var b = el('button', {type: 'button', 'class': 'pf-v6-c-button ' + classes});
    b.appendChild(el('span', {'class': 'pf-v6-c-button__text'}, label));
    b.addEventListener('click', onClick);
    return b;
  }

  // callAPI sends a same-origin request and returns {ok, status, data, error}. A JSON body
  // is sent when body is given. Errors carry the server's message when it sent one.
  async function callAPI(method, url, body) {
    var init = {method: method, headers: {}};
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    var resp;
    try {
      resp = await fetch(url, init);
    } catch (e) {
      return {ok: false, status: 0, error: 'Network error'};
    }
    var text = await resp.text();
    var data = null;
    try { data = JSON.parse(text); } catch (e) { /* not JSON */ }
    var error = resp.ok ? '' : (data && data.error) || text || 'HTTP ' + resp.status;
    return {ok: resp.ok, status: resp.status, data: data, error: error};
  }

  // versionTime decodes the creation time of a version id. Version ids are xids: 12 bytes
  // in base32hex, the first four being the Unix time in seconds, i.e. the top 32 of the
  // first 7 characters' 35 bits.
  function versionTime(id) {
    var n = 0;
    for (var i = 0; i < 7; i++) { n = n * 32 + parseInt(id.charAt(i), 32); }
    return new Date(Math.floor(n / 8) * 1000);
  }

  function setupManage() {
    var root = document.getElementById('manage');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var alerts = $('manage-alert');
    var siteName = new URLSearchParams(location.search).get('site');
    var user = null;

    function show(id) {
      ['manage-loading', 'manage-signin', 'manage-list', 'manage-detail'].forEach(function (x) {
        $(x).hidden = x !== id;
      });
    }
    function siteURL(name) {
      return root.getAttribute('data-site-origin').replace('{site}', name) + '/';
    }
    function groupName(id) {
      if (!id) { return 'None'; }
      var g = user.groups.find(function (x) { return x.id === id; });
      return g ? g.name : 'Group ' + id;
    }
    function fillSelects() {
      document.querySelectorAll('.js-visibility').forEach(function (sel) {
        sel.replaceChildren();
        Object.keys(visibilityLabels).forEach(function (v) {
          sel.appendChild(el('option', {value: v}, visibilityLabels[v]));
        });
      });
      document.querySelectorAll('.js-groups').forEach(function (sel) {
        sel.replaceChildren(el('option', {value: '0'}, 'None'));
        user.groups.forEach(function (g) { sel.appendChild(el('option', {value: String(g.id)}, g.name)); });
      });
    }
    async function loadUser() {
      var r = await callAPI('GET', '/v1/auth/user');
      if (r.status === 401) { show('manage-signin'); return false; }
      if (!r.ok) {
        show(null);
        showAlert(alerts, 'danger', 'Could not load your sites.', r.error);
        return false;
      }
      user = r.data;
      fillSelects();
      return true;
    }

    // -- the list --
    function renderList() {
      var sites = user.docs.owned;
      var body = $('owned-body');
      body.replaceChildren();
      $('owned-table').hidden = sites.length === 0;
      $('owned-empty').hidden = sites.length !== 0;
      sites.forEach(function (d) {
        var tr = el('tr', {'class': 'pf-v6-c-table__tr'});
        var name = el('td', {'class': 'pf-v6-c-table__td', 'data-label': 'Site'});
        name.appendChild(el('a', {href: '/manage?site=' + encodeURIComponent(d.name)}, d.name));
        tr.appendChild(name);
        tr.appendChild(el('td', {'class': 'pf-v6-c-table__td', 'data-label': 'Visibility'}, visibilityLabels[d.visibility] || d.visibility));
        tr.appendChild(el('td', {'class': 'pf-v6-c-table__td', 'data-label': 'Group'}, groupName(d.group_id)));
        tr.appendChild(el('td', {'class': 'pf-v6-c-table__td', 'data-label': 'Description'}, d.description));
        var actions = el('td', {'class': 'pf-v6-c-table__td pf-m-actions'});
        actions.appendChild(el('a', {'class': 'pf-v6-c-button pf-m-secondary pf-m-small', href: siteURL(d.name)}, 'Open'));
        tr.appendChild(actions);
        body.appendChild(tr);
      });
    }
    function setupCreate() {
      var form = $('create-form');
      form.addEventListener('submit', async function (evt) {
        evt.preventDefault();
        var name = form.elements.name.value;
        var r = await callAPI('POST', '/v1/auth/sites', {
          name: name,
          description: form.elements.description.value,
          visibility: form.elements.visibility.value,
          group: parseInt(form.elements.group.value, 10)
        });
        if (!r.ok) { showAlert(alerts, 'danger', 'Could not create the site.', r.error); return; }
        location.assign('/manage?site=' + encodeURIComponent(name));
      });
    }

    // -- one site --
    function selectValue(sel, value) {
      // A group the user has since left is not in the list; keep its id selectable.
      if (!Array.prototype.some.call(sel.options, function (o) { return o.value === value; })) {
        sel.appendChild(el('option', {value: value}, 'Group ' + value));
      }
      sel.value = value;
    }
    function renderSettings(site) {
      var form = $('settings-form');
      form.elements.description.value = site.description;
      form.elements.visibility.value = site.visibility;
      selectValue(form.elements.group, String(site.group));
    }
    function renderVersions(v) {
      var body = $('versions-body');
      body.replaceChildren();
      $('versions-table').hidden = v.versions.length === 0;
      $('versions-empty').hidden = v.versions.length !== 0;
      v.versions.forEach(function (id) {
        var tr = el('tr', {'class': 'pf-v6-c-table__tr'});
        tr.appendChild(el('td', {'class': 'pf-v6-c-table__td', 'data-label': 'Version'}, id));
        tr.appendChild(el('td', {'class': 'pf-v6-c-table__td', 'data-label': 'Deployed'}, versionTime(id).toLocaleString()));
        var status = el('td', {'class': 'pf-v6-c-table__td', 'data-label': 'Status'});
        var actions = el('td', {'class': 'pf-v6-c-table__td pf-m-actions'});
        if (id === v.current) {
          var label = el('span', {'class': 'pf-v6-c-label pf-m-green pf-m-compact'});
          label.appendChild(el('span', {'class': 'pf-v6-c-label__content'}, 'Live'));
          status.appendChild(label);
        } else {
          actions.appendChild(buttonEl('Roll back to this version', 'pf-m-secondary pf-m-small', function () { rollback(id); }));
        }
        tr.appendChild(status);
        tr.appendChild(actions);
        body.appendChild(tr);
      });
    }
    async function loadVersions() {
      var r = await callAPI('GET', '/v1/auth/sites/' + encodeURIComponent(siteName) + '/versions');
      if (!r.ok) { showAlert($('versions-result'), 'danger', 'Could not load the versions.', r.error); return; }
      $('versions-result').replaceChildren();
      renderVersions(r.data);
    }
    async function rollback(id) {
      if (!confirm('Make version ' + id + ' the live version of ' + siteName + '?')) { return; }
      var r = await callAPI('POST', '/v1/auth/sites/' + encodeURIComponent(siteName) + '/rollback', {version: id});
      if (!r.ok) { showAlert($('versions-result'), 'danger', 'Rollback failed.', r.error); return; }
      await loadVersions();
      showAlert($('versions-result'), 'success', 'Rolled back.', 'Version ' + id + ' is live.');
    }
    function setupDeploy() {
      var form = $('deploy-form');
      var result = $('deploy-result');
      var progress = $('deploy-progress');
      var bar = progress.querySelector('[role=progressbar]');
      var indicator = $('deploy-progress-bar');
      var measure = $('deploy-progress-measure');
      function setProgress(pct) {
        var p = Math.max(0, Math.min(100, Math.round(pct)));
        indicator.style.width = p + '%';
        bar.setAttribute('aria-valuenow', String(p));
        bar.setAttribute('aria-valuetext', p + '%');
        measure.textContent = p + '%';
      }
      form.addEventListener('submit', function (evt) {
        evt.preventDefault();
        var file = form.elements.file.files[0];
        result.replaceChildren();
        if (!file) { showAlert(result, 'danger', 'Choose an archive to upload.'); return; }
        setProgress(0);
        progress.hidden = false;
        var data = new FormData();
        data.append('file', file);
        var xhr = new XMLHttpRequest();
        xhr.open('POST', '/v1/auth/sites/' + encodeURIComponent(siteName) + '/formupload');
        xhr.upload.onprogress = function (e) { if (e.lengthComputable) { setProgress(e.loaded / e.total * 100); } };
        xhr.onload = function () {
          if (xhr.status >= 200 && xhr.status < 300) {
            setProgress(100);
            showAlert(result, 'success', 'Upload complete.', xhr.responseText);
            form.reset();
            loadVersions();
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
    function setupSettings() {
      var form = $('settings-form');
      form.addEventListener('submit', async function (evt) {
        evt.preventDefault();
        var r = await callAPI('PUT', '/v1/auth/sites/' + encodeURIComponent(siteName), {
          description: form.elements.description.value,
          visibility: form.elements.visibility.value,
          group: parseInt(form.elements.group.value, 10)
        });
        if (r.ok) { showAlert($('settings-result'), 'success', 'Settings saved.'); } else { showAlert($('settings-result'), 'danger', 'Could not save the settings.', r.error); }
      });
    }
    function setupDelete() {
      $('delete-site').addEventListener('click', async function () {
        if (!confirm('Delete ' + siteName + ' and all of its versions? This cannot be undone.')) { return; }
        var r = await callAPI('DELETE', '/v1/auth/sites/' + encodeURIComponent(siteName));
        if (r.ok) { location.assign('/manage'); } else { showAlert($('delete-result'), 'danger', 'Could not delete the site.', r.error); }
      });
    }
    async function showSite() {
      var r = await callAPI('GET', '/v1/auth/sites/' + encodeURIComponent(siteName));
      var own = user.docs.owned.some(function (d) { return d.name === siteName; });
      if (!r.ok || !own) {
        show('manage-list');
        renderList();
        showAlert(alerts, 'danger', 'Site not found.', r.ok ? 'You do not own ' + siteName + '.' : r.error);
        return;
      }
      $('detail-crumb').textContent = siteName;
      $('detail-title').textContent = siteName;
      var link = $('detail-link');
      link.replaceChildren(el('a', {href: siteURL(siteName)}, siteURL(siteName)));
      renderSettings(r.data);
      show('manage-detail');
      loadVersions();
    }

    (async function () {
      setupCreate();
      setupDeploy();
      setupSettings();
      setupDelete();
      if (!await loadUser()) { return; }
      if (siteName) { await showSite(); } else { renderList(); show('manage-list'); }
    })();
  }

  setupTheme();
  setupNav();
  setupLogin();
  setupUpload();
  setupManage();
})();
