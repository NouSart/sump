import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import '@fontsource/fira-code/latin-400.css';
import '@fontsource/fira-code/latin-700.css';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('#root 缺失');

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
