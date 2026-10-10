// Applies the theme before the page is painted: the saved choice, else the system setting.
// Loaded without defer from <head>; app.js wires up the toggle button.
(function () {
  'use strict';
  var saved = null;
  try { saved = localStorage.getItem('open-pages-theme'); } catch (e) { /* storage blocked */ }
  var dark = saved ? saved === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches;
  document.documentElement.classList.toggle('pf-v6-theme-dark', dark);
})();
