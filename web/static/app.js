(() => {
  const button = document.querySelector('[data-nav-toggle]');
  const nav = document.querySelector('#primary-nav');
  const background = document.querySelector('[data-drawer-background]');
  if (!button || !nav || !background) return;

  const mobile = window.matchMedia('(max-width: 820px)');
  const firstLink = nav.querySelector('nav a');

  const sync = () => {
    const closed = mobile.matches && !document.body.classList.contains('nav-open');
    const expanded = mobile.matches && !closed;
    nav.inert = closed;
    background.inert = expanded;
    if (closed) nav.setAttribute('aria-hidden', 'true');
    else nav.removeAttribute('aria-hidden');
  };

  const close = (restoreFocus = false) => {
    document.body.classList.remove('nav-open');
    button.setAttribute('aria-expanded', 'false');
    button.querySelector('.sr-only').textContent = '메뉴 열기';
    sync();
    if (restoreFocus && mobile.matches) button.focus();
  };

  const open = () => {
    document.body.classList.add('nav-open');
    button.setAttribute('aria-expanded', 'true');
    button.querySelector('.sr-only').textContent = '메뉴 닫기';
    sync();
    firstLink?.focus();
  };

  button.addEventListener('click', () => {
    if (document.body.classList.contains('nav-open')) close(true);
    else open();
  });

  nav.addEventListener('click', (event) => {
    if (event.target.closest('a')) close(false);
  });

  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && document.body.classList.contains('nav-open')) close(true);
    if (event.key !== 'Tab' || !mobile.matches || !document.body.classList.contains('nav-open')) return;

    const focusable = [
      button,
      ...nav.querySelectorAll('a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])'),
    ];
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  });

  mobile.addEventListener('change', () => close(false));
  sync();
})();

(() => {
  const form = document.querySelector('[data-invite-token-form]');
  if (!form) return;
  const token = new URLSearchParams(window.location.hash.slice(1)).get('token');
  history.replaceState(null, '', window.location.pathname);
  if (!token) {
    const status = form.querySelector('[data-invite-token-status]');
    if (status) status.textContent = '초대 링크가 올바르지 않습니다. 관리자에게 재초대를 요청해 주세요.';
    return;
  }
  form.elements.token.value = token;
  form.requestSubmit();
})();

(() => {
  // Removal is destructive, so the browser asks before the form is submitted.
  // Without JavaScript the form still works and the server keeps its checks.
  for (const form of document.querySelectorAll('form[data-confirm]')) {
    form.addEventListener('submit', (event) => {
      if (!window.confirm(form.dataset.confirm)) event.preventDefault();
    });
  }
})();
