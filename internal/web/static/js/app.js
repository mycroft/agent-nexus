// Small client-side behaviors; everything else is driven by htmx attributes.
(() => {
  document.addEventListener('click', (e) => {
    if (e.target.closest('[data-theme-toggle]')) {
      toggleTheme();
      return;
    }
    // History rows open their job, unless a link or a text selection was
    // the point of the click.
    const row = e.target.closest('tr[data-href]');
    if (row && !e.target.closest('a') && !window.getSelection().toString()) {
      if (e.metaKey || e.ctrlKey) {
        window.open(row.dataset.href, '_blank');
      } else {
        window.location.href = row.dataset.href;
      }
    }
  });

  // The theme follows the OS until toggled; the choice is kept in a cookie so
  // the server renders it (<html data-theme>) on the next page load.
  const toggleTheme = () => {
    const root = document.documentElement;
    const current = root.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    const next = current === 'dark' ? 'light' : 'dark';
    root.dataset.theme = next;
    const secure = location.protocol === 'https:' ? '; Secure' : '';
    document.cookie = `theme=${next}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`;
  };
})();
