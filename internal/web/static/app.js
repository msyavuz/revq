// Drag a card to another column to pin it there until the author pushes again.
let dragged = null;

document.addEventListener('dragstart', (e) => {
  const card = e.target.closest?.('.card');
  if (!card) return;
  dragged = card;
  card.classList.add('dragging');
  document.body.classList.add('dragging');
  e.dataTransfer.effectAllowed = 'move';
  e.dataTransfer.setData('text/plain', card.dataset.id);
});

document.addEventListener('dragend', () => {
  dragged?.classList.remove('dragging');
  dragged = null;
  document.body.classList.remove('dragging');
  document.querySelectorAll('.col.over').forEach((c) => c.classList.remove('over'));
});

document.addEventListener('dragover', (e) => {
  const col = e.target.closest?.('.col');
  if (!dragged || !col || col.dataset.col === 'done') return;
  e.preventDefault();
  document.querySelectorAll('.col.over').forEach((c) => c !== col && c.classList.remove('over'));
  col.classList.add('over');
});

document.addEventListener('drop', (e) => {
  const col = e.target.closest?.('.col');
  if (!dragged || !col || col.dataset.col === 'done') return;
  e.preventDefault();
  if (dragged.closest('.col') === col) return;
  htmx.ajax('POST', `/pr/${dragged.dataset.id}/move`, {
    target: '#board',
    swap: 'innerHTML',
    values: { col: col.dataset.col },
  });
});

// Theme switch: Auto (follow the OS) -> Light -> Dark. Stored per browser.
const themeBtn = document.getElementById('theme');
if (themeBtn) {
  const order = ['auto', 'light', 'dark'];
  const label = { auto: 'Auto', light: 'Light', dark: 'Dark' };
  const current = () => document.documentElement.dataset.theme || 'auto';
  const show = () => {
    themeBtn.textContent = label[current()];
    themeBtn.setAttribute('aria-label', `Theme: ${label[current()]}. Switch theme`);
  };
  themeBtn.addEventListener('click', () => {
    const next = order[(order.indexOf(current()) + 1) % order.length];
    if (next === 'auto') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = next;
    try {
      if (next === 'auto') localStorage.removeItem('revq-theme');
      else localStorage.setItem('revq-theme', next);
    } catch (e) {}
    show();
  });
  show();
}
