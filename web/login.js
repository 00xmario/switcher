const form = document.getElementById('login-form');
const password = document.getElementById('login-password');
const submit = document.getElementById('login-submit');
const error = document.getElementById('login-error');

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  submit.disabled = true;
  submit.textContent = 'Checking…';
  error.textContent = '';
  try {
    const response = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      cache: 'no-store',
      body: JSON.stringify({ password: password.value }),
    });
    const result = await response.json().catch(() => ({}));
    if (!response.ok) {
      error.textContent = result.error || 'Could not unlock Switcher';
      return;
    }
    if (result.csrf) localStorage.setItem('switcher-csrf', result.csrf);
    password.value = '';
    location.replace('/');
  } catch {
    error.textContent = 'Could not reach Switcher';
  } finally {
    submit.disabled = false;
    submit.textContent = 'Unlock Switcher';
  }
});
