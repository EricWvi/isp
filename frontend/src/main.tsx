import React from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import './style.css'

const systemDarkMode = window.matchMedia('(prefers-color-scheme: dark)')
const applySystemTheme = () => {
  document.documentElement.classList.toggle('dark', systemDarkMode.matches)
}
applySystemTheme()
systemDarkMode.addEventListener('change', applySystemTheme)

createRoot(document.getElementById('root')!).render(
  <React.StrictMode><App /></React.StrictMode>,
)
